// SPDX-License-Identifier: GPL-3.0-only

package main

// Voicemail on the stick: an incoming call nobody takes (after N seconds, or
// right away) is answered here, a greeting is played (an uploaded/recorded WAV,
// a beep, or nothing), and the caller is recorded until they hang up or the
// maximum length is reached. Taking the call means the carrier's own voicemail
// never kicks in.
//
// dongie (unprivileged) writes the settings and the greeting into its state
// dir; recordings are stored as G.722 (own codec, ~480 KB/min) on flash when
// dongie's `persist` is on, else in RAM, and served as WAV on request.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dongie-ims/g722"

	ims "github.com/damonto/ims-go"
	imsvoice "github.com/damonto/ims-go/ims/voice"
)

const (
	vmMaxCount   = 30
	vmMaxBytes   = 20 << 20
	vmRAMDir     = "/run/dongie-ims/voicemail"
	greetMaxSecs = 60
)

// vmConfig is written by dongie (ims-voicemail.json).
type vmConfig struct {
	Mode     string `json:"mode"`       // off | delay | now
	Delay    int    `json:"delay_secs"` // for "delay": ring this long in the browser first
	Greeting string `json:"greeting"`   // none | beep | file
	MaxSecs  int    `json:"max_secs"`   // longest recording
	Persist  bool   `json:"persist"`    // store on flash (dongie's persist)
}

func readVMConfig(path string) vmConfig {
	c := vmConfig{Mode: "off", Delay: 20, Greeting: "beep", MaxSecs: 120}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Mode != "delay" && c.Mode != "now" {
		c.Mode = "off"
	}
	c.Delay = min(max(c.Delay, 1), 120)
	c.MaxSecs = min(max(c.MaxSecs, 10), 300)
	if c.Greeting != "none" && c.Greeting != "file" {
		c.Greeting = "beep"
	}
	return c
}

// readGreeting loads a WAV: PCM 16-bit, mono, 16 kHz (dongie's page converts
// whatever was uploaded or recorded to exactly that), at most greetMaxSecs.
func readGreeting(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("greeting: not a WAV file")
	}
	var data []byte
	fmtOK := false
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), int(binary.LittleEndian.Uint32(b[off+4:off+8]))
		body := b[off+8 : min(off+8+size, len(b))]
		switch id {
		case "fmt ":
			if len(body) >= 16 {
				format, ch := binary.LittleEndian.Uint16(body[0:2]), binary.LittleEndian.Uint16(body[2:4])
				rate, bits := binary.LittleEndian.Uint32(body[4:8]), binary.LittleEndian.Uint16(body[14:16])
				fmtOK = format == 1 && ch == 1 && rate == 16000 && bits == 16
			}
		case "data":
			data = body
		}
		off += 8 + size + size%2
	}
	if !fmtOK {
		return nil, errors.New("greeting: need PCM 16-bit mono 16 kHz")
	}
	n := min(len(data)/2, greetMaxSecs*16000)
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return pcm, nil
}

func beep() []int16 {
	pcm := make([]int16, 16000*4/10) // 400 ms, 1 kHz, soft edges
	for i := range pcm {
		env := math.Min(1, math.Min(float64(i), float64(len(pcm)-i))/160)
		pcm[i] = int16(9000 * env * math.Sin(2*math.Pi*1000*float64(i)/16000))
	}
	return pcm
}

// vmMeta sits next to each recording (<id>.json).
type vmMeta struct {
	ID     string `json:"id"`
	CallID uint64 `json:"call"`
	From   string `json:"from"`
	At     int64  `json:"at"`
	Secs   int    `json:"secs"`
	Codec  string `json:"codec"`
	Stored string `json:"stored"` // flash | ram
}

type voicemail struct {
	st         *state
	log        *slog.Logger
	configPath string
	greeting   string
	flashDir   string
}

func (v *voicemail) dirs() []string { return []string{v.flashDir, vmRAMDir} }

// maybeTake runs for every incoming call (unless calls are rejected).
func (v *voicemail) maybeTake(ctx context.Context, id uint64, in ims.IncomingCall) {
	cfg := readVMConfig(v.configPath)
	if cfg.Mode == "off" {
		return
	}
	if cfg.Mode == "delay" {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(cfg.Delay) * time.Second):
		}
	}
	call, ok := v.st.claimRinging(id)
	if !ok {
		return // answered in the browser, rejected, or the caller gave up
	}
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := call.Answer(actx, imsvoice.MediaOffer{Codecs: []imsvoice.AudioCodec{imsvoice.CodecAMRWB, imsvoice.CodecAMR}})
	cancel()
	if err != nil {
		v.log.Warn("voicemail: answering failed", "id", id, "err", err)
		return
	}
	v.st.markVoicemail(id)
	v.log.Info("voicemail: call taken", "id", id, "from", maskNumber(in.FromNumber))
	meta, err := v.record(ctx, call, cfg, id, in.FromNumber)
	hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = call.Hangup(hctx)
	hcancel()
	if err != nil {
		v.log.Warn("voicemail: recording", "id", id, "err", err)
	}
	if meta != nil {
		v.st.setVoicemail(id, meta.ID, meta.Secs)
		v.log.Info("voicemail: saved", "id", id, "secs", meta.Secs, "stored", meta.Stored)
	}
}

func (v *voicemail) record(ctx context.Context, call *imsvoice.Call, cfg vmConfig, id uint64, from string) (*vmMeta, error) {
	codec, err := newIMSCodec(call.Media())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// sender: greeting, beep, then silence (keeps RTP flowing: some networks
	// end a call that sends nothing), 20 ms per packet, own SSRC
	var out []int16
	switch cfg.Greeting {
	case "file":
		if g, err := readGreeting(v.greeting); err == nil {
			out = append(out, g...)
		} else {
			v.log.Warn("voicemail: greeting file", "err", err)
		}
		out = append(out, make([]int16, 4800)...) // 0.3 s gap
		out = append(out, beep()...)
	case "beep":
		out = append(out, beep()...)
	}
	greetingDone := make(chan struct{})
	go func() {
		seq, ts, ssrc := uint16(randU32()), randU32(), randU32()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		silence := make([]int16, frame16k)
		pos, signalled := 0, false
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			frame := silence
			if pos < len(out) {
				end := min(pos+frame16k, len(out))
				frame = append(append([]int16(nil), out[pos:end]...), make([]int16, frame16k-(end-pos))...)
				pos = end
			} else if !signalled {
				close(greetingDone)
				signalled = true
			}
			payload, err := codec.encode(frame)
			if err != nil {
				continue
			}
			pkt := imsvoice.RTPPacket{PayloadType: codec.media.PayloadType, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Payload: payload}
			seq++
			ts += uint32(codec.clock() / 50)
			if raw, err := pkt.MarshalBinary(); err == nil {
				_ = call.WriteRTP(ctx, raw)
			}
		}
	}()

	// receiver: only what comes after the greeting is the message
	ended := time.NewTicker(250 * time.Millisecond)
	defer ended.Stop()
wait:
	for {
		select {
		case <-greetingDone:
			break wait
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ended.C:
			if v.st.callEnded(id) {
				return nil, nil // hung up during the greeting
			}
		}
	}
	vmID := fmt.Sprintf("%d-%d", time.Now().Unix(), id)
	dir := vmRAMDir
	stored := "ram"
	if cfg.Persist {
		dir, stored = v.flashDir, "flash"
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, vmID+".g722"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	enc := g722.NewEncoder()
	samples := 0
	// RTP timestamps keep counting through silence (DTX: the network sends
	// next to nothing while the caller is quiet); fill those gaps so pauses
	// stay pauses and the length is the real one
	var ts0 uint32
	haveTS := false
	perSample := float64(16000) / float64(codec.clock())
	silence := make([]int16, frame16k)
	rctx, rcancel := context.WithTimeout(ctx, time.Duration(cfg.MaxSecs)*time.Second)
	defer rcancel()
	pt := codec.media.PayloadType
	for {
		raw, _, err := call.ReadRTP(rctx)
		if err != nil {
			break // caller hung up, or max length reached
		}
		var p imsvoice.RTPPacket
		if len(raw) < 12 || raw[0]>>6 != 2 || p.UnmarshalBinary(raw) != nil || p.PayloadType != pt {
			continue
		}
		if !haveTS {
			ts0, haveTS = p.Timestamp, true
		}
		if want := int(float64(p.Timestamp-ts0) * perSample); want > samples && want-samples < 16000*30 {
			for gap := want - samples; gap > 0; gap -= frame16k {
				n := min(gap, frame16k)
				samples += n
				_, _ = f.Write(enc.Encode(make([]byte, 0, 160), silence[:n]))
			}
		}
		frames, _ := codec.decode(p.Payload)
		for _, pcm := range frames {
			samples += len(pcm)
			_, _ = f.Write(enc.Encode(make([]byte, 0, 160), pcm))
		}
	}
	_ = f.Close()
	secs := samples / 16000
	if secs < 1 {
		_ = os.Remove(filepath.Join(dir, vmID+".g722"))
		return nil, nil // hung up right after the beep: nothing to keep
	}
	meta := &vmMeta{ID: vmID, CallID: id, From: from, At: time.Now().Unix() - int64(secs), Secs: secs, Codec: codec.name(), Stored: stored}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, vmID+".json"), b, 0o640); err != nil {
		return nil, err
	}
	v.prune()
	return meta, nil
}

// list returns the stored voicemails, newest first.
func (v *voicemail) list() []vmMeta {
	var out []vmMeta
	for _, d := range v.dirs() {
		paths, _ := filepath.Glob(filepath.Join(d, "*.json"))
		for _, p := range paths {
			var m vmMeta
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &m) == nil && validVMID(m.ID) {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

func validVMID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func (v *voicemail) find(id string) (string, bool) {
	if !validVMID(id) {
		return "", false
	}
	for _, d := range v.dirs() {
		p := filepath.Join(d, id+".g722")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// wav decodes a stored recording to a 16 kHz mono WAV.
func (v *voicemail) wav(id string) ([]byte, error) {
	p, ok := v.find(id)
	if !ok {
		return nil, os.ErrNotExist
	}
	code, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	pcm := g722.NewDecoder().Decode(make([]int16, 0, len(code)*2), code)
	out := make([]byte, 44+2*len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+2*len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], 16000)
	binary.LittleEndian.PutUint32(out[28:], 32000)
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(2*len(pcm)))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(out[44+2*i:], uint16(s))
	}
	return out, nil
}

func (v *voicemail) remove(id string) bool {
	p, ok := v.find(id)
	if !ok {
		return false
	}
	_ = os.Remove(p)
	_ = os.Remove(strings.TrimSuffix(p, ".g722") + ".json")
	return true
}

// prune keeps at most vmMaxCount recordings / vmMaxBytes per place, oldest out first.
func (v *voicemail) prune() {
	for _, d := range v.dirs() {
		paths, _ := filepath.Glob(filepath.Join(d, "*.g722"))
		type f struct {
			p    string
			size int64
			mod  time.Time
		}
		var fs []f
		var total int64
		for _, p := range paths {
			if st, err := os.Stat(p); err == nil {
				fs = append(fs, f{p, st.Size(), st.ModTime()})
				total += st.Size()
			}
		}
		sort.Slice(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) })
		for len(fs) > 0 && (len(fs) > vmMaxCount || total > vmMaxBytes) {
			_ = os.Remove(fs[0].p)
			_ = os.Remove(strings.TrimSuffix(fs[0].p, ".g722") + ".json")
			total -= fs[0].size
			fs = fs[1:]
		}
	}
}
