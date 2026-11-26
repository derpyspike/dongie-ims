// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"dongie-ims/g722"
)

func wavPCM(rate uint32, ch uint16, pcm []int16) []byte {
	b := make([]byte, 44+2*len(pcm))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], ch)
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(pcm)))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(s))
	}
	return b
}

func TestVMConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vm.json")
	if c := readVMConfig(p); c.Mode != "off" || c.Greeting != "beep" || c.MaxSecs != 120 {
		t.Fatalf("defaults %+v", c)
	}
	_ = os.WriteFile(p, []byte(`{"mode":"delay","delay_secs":0,"greeting":"file","max_secs":9999,"persist":true}`), 0o644)
	c := readVMConfig(p)
	if c.Mode != "delay" || c.Delay != 1 || c.Greeting != "file" || c.MaxSecs != 300 || !c.Persist {
		t.Fatalf("clamped %+v", c)
	}
	_ = os.WriteFile(p, []byte(`{"mode":"rm -rf","greeting":"x"}`), 0o644)
	if c := readVMConfig(p); c.Mode != "off" || c.Greeting != "beep" {
		t.Fatalf("garbage %+v", c)
	}
}

func TestGreetingWAV(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "g.wav")
	_ = os.WriteFile(good, wavPCM(16000, 1, make([]int16, 16000)), 0o644)
	if pcm, err := readGreeting(good); err != nil || len(pcm) != 16000 {
		t.Fatalf("good: %d %v", len(pcm), err)
	}
	bad := filepath.Join(dir, "b.wav")
	_ = os.WriteFile(bad, wavPCM(44100, 2, make([]int16, 100)), 0o644)
	if _, err := readGreeting(bad); err == nil {
		t.Fatal("44.1 kHz stereo accepted")
	}
	long := filepath.Join(dir, "l.wav")
	_ = os.WriteFile(long, wavPCM(16000, 1, make([]int16, 16000*(greetMaxSecs+5))), 0o644)
	if pcm, _ := readGreeting(long); len(pcm) != 16000*greetMaxSecs {
		t.Fatalf("not capped: %d", len(pcm))
	}
}

func TestStoreWAVAndPrune(t *testing.T) {
	dir := t.TempDir()
	v := &voicemail{flashDir: dir}
	pcm := make([]int16, 16000) // 1 s
	code := g722.NewEncoder().Encode(nil, pcm)
	_ = os.WriteFile(filepath.Join(dir, "100-1.g722"), code, 0o640)
	_ = os.WriteFile(filepath.Join(dir, "100-1.json"), []byte(`{"id":"100-1","at":100,"secs":1}`), 0o640)
	w, err := v.wav("100-1")
	if err != nil || string(w[0:4]) != "RIFF" || binary.LittleEndian.Uint32(w[24:]) != 16000 || len(w) != 44+2*16000 {
		t.Fatalf("wav: %v len %d", err, len(w))
	}
	if _, err := v.wav("../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if l := v.list(); len(l) != 1 || l[0].ID != "100-1" {
		t.Fatalf("list %+v", l)
	}
	for i := 0; i < vmMaxCount+3; i++ {
		id := filepath.Join(dir, "200-"+string(rune('a'+i)))
		_ = os.WriteFile(id+".g722", []byte{1}, 0o640)
	}
	v.prune()
	n, _ := filepath.Glob(filepath.Join(dir, "*.g722"))
	if len(n) != vmMaxCount {
		t.Fatalf("prune left %d", len(n))
	}
	if !v.remove("100-1") && len(n) == vmMaxCount {
		// 100-1 may have been pruned as the oldest; either way it must be gone now
	}
	if _, ok := v.find("100-1"); ok {
		t.Fatal("still there")
	}
}
