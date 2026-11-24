// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"math"
	"testing"

	imsvoice "github.com/damonto/ims-go/ims/voice"
)

func sine(n int, f float64, start int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(8000 * math.Sin(2*math.Pi*f*float64(start+i)/16000))
	}
	return out
}

func rms(x []int16) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

// Encoding to an AMR RTP payload and decoding it back must give speech-band
// audio with roughly the input level, for both packings and both codecs.
func TestIMSCodecRoundTrip(t *testing.T) {
	for _, m := range []imsvoice.NegotiatedMedia{
		{AudioFormat: imsvoice.AudioFormat{Codec: imsvoice.CodecAMRWB, PayloadType: 96, ClockRate: 16000, ModeSet: "0,1,2"}},
		{AudioFormat: imsvoice.AudioFormat{Codec: imsvoice.CodecAMRWB, PayloadType: 96, ClockRate: 16000, OctetAlign: true}},
		{AudioFormat: imsvoice.AudioFormat{Codec: imsvoice.CodecAMR, PayloadType: 97, ClockRate: 8000, ModeSet: "0,2,4,7"}},
	} {
		enc, err := newIMSCodec(m)
		if err != nil {
			t.Fatal(err)
		}
		dec, _ := newIMSCodec(m)
		var in, out []int16
		for f := 0; f < 50; f++ {
			pcm := sine(frame16k, 700, f*frame16k)
			in = append(in, pcm...)
			p, err := enc.encode(pcm)
			if err != nil {
				t.Fatalf("%s encode: %v", enc.name(), err)
			}
			frames, err := dec.decode(p)
			if err != nil || len(frames) != 1 || len(frames[0]) != frame16k {
				t.Fatalf("%s decode: %v (%d frames)", enc.name(), err, len(frames))
			}
			out = append(out, frames[0]...)
		}
		ri, ro := rms(in[8000:]), rms(out[8000:])
		t.Logf("%s octet-align=%v: rms in %.0f out %.0f", enc.name(), m.OctetAlign, ri, ro)
		if ro < ri*0.5 || ro > ri*1.5 {
			t.Errorf("%s: level off (in %.0f, out %.0f)", enc.name(), ri, ro)
		}
	}
}

func TestHighestMode(t *testing.T) {
	if highestMode("0,1,2", 8, 8) != 2 || highestMode("", 2, 8) != 2 || highestMode("0,2,4,7", 7, 7) != 7 || highestMode("8,9", 2, 8) != 8 {
		t.Fatal("mode-set parsing")
	}
}
