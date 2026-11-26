// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	ims "github.com/damonto/ims-go"
	imsvoice "github.com/damonto/ims-go/ims/voice"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"dongie-ims/g722"
)

// The browser leg: one WebRTC peer connection (G.722) bridged to one IMS call
// (AMR-WB / AMR). Only one at a time. Signalling arrives through dongie
// (admin login + CSRF); media is DTLS-SRTP on UDP 40100-40199, offered only on
// tailnet / hotspot / USB addresses.

var bridgeNets = []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48", "192.168.4.0/24", "192.168.5.0/24"}

type bridge struct {
	log *slog.Logger
	api *webrtc.API
	st  *state

	mu   sync.Mutex
	sess *session
}

type session struct {
	b       *bridge
	entry   uint64 // call log id
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticRTP
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	call    *imsvoice.Call
	codec   *imsCodec
	ssrc    uint32
	seq     uint16
	ts      uint32
	closing bool
}

func newBridge(st *state, log *slog.Logger) (*bridge, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000},
		PayloadType:        9,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	var nets []*net.IPNet
	for _, c := range bridgeNets {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(40100, 40199); err != nil {
		return nil, err
	}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetIPFilter(func(ip net.IP) bool {
		for _, n := range nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	})
	return &bridge{log: log, st: st, api: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))}, nil
}

func randU32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// open creates the peer connection from the browser's offer and returns our answer.
func (b *bridge) open(offer string, entry uint64) (*session, string, error) {
	b.mu.Lock()
	if b.sess != nil {
		b.mu.Unlock()
		return nil, "", errors.New("another call is already connected to a browser")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{b: b, entry: entry, ctx: ctx, cancel: cancel, ssrc: randU32(), seq: uint16(randU32()), ts: randU32()}
	b.sess = s
	b.mu.Unlock()

	answer, err := s.setup(offer)
	if err != nil {
		s.close("setup failed")
		return nil, "", err
	}
	return s, answer, nil
}

func (s *session) setup(offer string) (string, error) {
	pc, err := s.b.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	s.pc = pc
	s.track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeG722, ClockRate: 8000}, "audio", "dongie")
	if err != nil {
		return "", err
	}
	sender, err := pc.AddTrack(s.track)
	if err != nil {
		return "", err
	}
	go func() { // drain RTCP
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if !strings.EqualFold(tr.Codec().MimeType, webrtc.MimeTypeG722) {
			s.b.log.Warn("browser sends unexpected codec", "mime", tr.Codec().MimeType)
			return
		}
		go s.fromBrowser(tr)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.b.log.Info("browser leg", "id", s.entry, "state", st.String())
		s.b.st.setBrowser(s.entry, st.String())
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go s.close("browser " + st.String())
		case webrtc.PeerConnectionStateDisconnected:
			go func() { // give ICE 8 s to recover before hanging up
				time.Sleep(8 * time.Second)
				if pc.ConnectionState() == webrtc.PeerConnectionStateDisconnected {
					s.close("browser disconnected")
				}
			}()
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", fmt.Errorf("browser offer: %w", err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("answer (does the browser offer G.722?): %w", err)
	}
	if !strings.Contains(ans.SDP, "G722") {
		return "", errors.New("browser did not offer G.722")
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}
	return pc.LocalDescription().SDP, nil
}

// attach connects an IMS call whose media is negotiated and starts the pumps.
func (s *session) attach(call *imsvoice.Call) error {
	m := call.Media()
	codec, err := newIMSCodec(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.call, s.codec = call, codec
	s.mu.Unlock()
	s.b.st.setCodec(s.entry, codec.name())
	s.b.log.Info("call bridged to browser", "id", s.entry, "codec", codec.name(), "octet_align", m.OctetAlign)
	go s.toBrowser(call, m.PayloadType)
	return nil
}

// toBrowser: IMS RTP (AMR) -> PCM -> G.722 -> WebRTC.
func (s *session) toBrowser(call *imsvoice.Call, pt int) {
	enc := g722.NewEncoder()
	var (
		in0      uint32
		have0    bool
		outSeq   = uint16(randU32())
		outBase  = randU32()
		codec    = s.currentCodec()
		divisor  = uint32(codec.clock() / 8000) // G.722 RTP clock is 8000 by RFC quirk
		bad, all int
	)
	for {
		raw, _, err := call.ReadRTP(s.ctx)
		if err != nil {
			return
		}
		var p imsvoice.RTPPacket
		if len(raw) < 12 || raw[0]>>6 != 2 || p.UnmarshalBinary(raw) != nil || p.PayloadType != pt {
			continue // RTCP, DTMF events, junk
		}
		if !have0 {
			in0, have0 = p.Timestamp, true
		}
		frames, err := codec.decode(p.Payload)
		all++
		if err != nil {
			bad++
			if bad <= 3 {
				s.b.log.Warn("AMR decode", "err", err)
			}
		}
		ts := outBase + (p.Timestamp-in0)/divisor
		for i, pcm := range frames {
			pkt := &rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 9, SequenceNumber: outSeq, Timestamp: ts + uint32(i*160)},
				Payload: enc.Encode(make([]byte, 0, 160), pcm),
			}
			outSeq++
			if err := s.track.WriteRTP(pkt); err != nil && !errors.Is(err, net.ErrClosed) {
				s.b.log.Debug("write to browser", "err", err)
			}
		}
	}
}

func (s *session) currentCodec() *imsCodec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codec
}

// fromBrowser: WebRTC G.722 -> PCM -> AMR -> IMS RTP. Before the IMS call is
// up (still ringing / dialing) the audio is dropped.
func (s *session) fromBrowser(tr *webrtc.TrackRemote) {
	dec := g722.NewDecoder()
	var pcm []int16
	for {
		p, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		pcm = dec.Decode(pcm, p.Payload)
		for len(pcm) >= frame16k {
			s.sendToIMS(pcm[:frame16k])
			pcm = append(pcm[:0], pcm[frame16k:]...)
		}
	}
}

func (s *session) sendToIMS(frame []int16) {
	s.mu.Lock()
	call, codec := s.call, s.codec
	if call == nil || codec == nil {
		s.mu.Unlock()
		return
	}
	payload, err := codec.encode(frame)
	if err != nil {
		s.mu.Unlock()
		return
	}
	pkt := imsvoice.RTPPacket{PayloadType: codec.media.PayloadType, SequenceNumber: s.seq, Timestamp: s.ts, SSRC: s.ssrc, Payload: payload}
	s.seq++
	s.ts += uint32(codec.clock() / 50)
	s.mu.Unlock()
	raw, err := pkt.MarshalBinary()
	if err != nil {
		return
	}
	_ = call.WriteRTP(s.ctx, raw)
}

// close tears down the browser leg and hangs up the IMS call (if any).
func (s *session) close(reason string) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	call := s.call
	s.mu.Unlock()
	s.b.log.Info("browser leg closed", "id", s.entry, "reason", reason)
	s.cancel()
	if call != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = call.Hangup(ctx)
		cancel()
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
	s.b.st.setBrowser(s.entry, "")
	s.b.mu.Lock()
	if s.b.sess == s {
		s.b.sess = nil
	}
	s.b.mu.Unlock()
}

func (b *bridge) current() *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess
}

// callEnded is called from the event loop when an IMS call ended.
func (b *bridge) callEnded(entry uint64) {
	if s := b.current(); s != nil && s.entry == entry {
		s.mu.Lock()
		s.call = nil // already over, no BYE needed
		s.mu.Unlock()
		s.close("call ended")
	}
}

// answer: browser picks up a ringing incoming call.
func (b *bridge) answer(id uint64, offer string) (string, error) {
	call, ok := b.st.claimRinging(id) // not also taken by the voicemail
	if !ok {
		return "", errors.New("no such ringing call (or the voicemail took it)")
	}
	s, sdp, err := b.open(offer, id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	if err := call.Answer(ctx, imsvoice.MediaOffer{Codecs: []imsvoice.AudioCodec{imsvoice.CodecAMRWB, imsvoice.CodecAMR}}); err != nil {
		s.close("answer failed")
		return "", fmt.Errorf("answering: %w", err)
	}
	if err := s.attach(call); err != nil {
		s.close(err.Error())
		return "", err
	}
	return sdp, nil
}

// dial: browser starts an outgoing call. Returns at once; the call proceeds
// in the background (ringing -> answered / failed).
func (b *bridge) dial(client *ims.Client, to, offer string) (uint64, string, error) {
	id := b.st.callOutgoing(to)
	s, sdp, err := b.open(offer, id)
	if err != nil {
		b.st.callFailed(id, err.Error())
		return 0, "", err
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		defer cancel()
		call, err := client.Voice().Dial(ctx, imsvoice.DialRequest{To: to, Media: imsvoice.MediaOffer{Codecs: []imsvoice.AudioCodec{imsvoice.CodecAMRWB, imsvoice.CodecAMR}}})
		if err != nil {
			b.log.Info("outgoing call not connected", "id", id, "err", err)
			b.st.callFailed(id, shortErr(err))
			s.close("dial ended")
			return
		}
		b.st.callConnected(id, call)
		if err := s.attach(call); err != nil {
			s.close(err.Error())
		}
	}()
	return id, sdp, nil
}

// hangup ends the browser call (or cancels dialing).
func (b *bridge) hangup(id uint64) bool {
	if s := b.current(); s != nil && s.entry == id {
		s.close("hung up by user")
		return true
	}
	return false
}

func (b *bridge) dtmf(id uint64, digits string) error {
	s := b.current()
	if s == nil || s.entry != id {
		return errors.New("call is not connected to the browser")
	}
	s.mu.Lock()
	call := s.call
	s.mu.Unlock()
	if call == nil {
		return errors.New("call not answered yet")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return call.SendDTMF(ctx, imsvoice.DTMFRequest{Digits: digits})
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s) > 120 {
		s = s[i+2:]
	}
	return s
}
