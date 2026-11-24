// SPDX-License-Identifier: GPL-3.0-only
package g722

import (
	"math"
	"testing"
)

// snr finds the best alignment (QMF delay) and returns the SNR in dB.
func snr(in, out []int16) (float64, int) {
	best, bestLag := -100.0, 0
	for lag := 0; lag < 64; lag++ {
		var sig, noise float64
		for i := 2000; i+lag < len(out) && i < len(in); i++ {
			s := float64(in[i])
			n := s - float64(out[i+lag])
			sig += s * s
			noise += n * n
		}
		if noise == 0 {
			return 200, lag
		}
		if v := 10 * math.Log10(sig/noise); v > best {
			best, bestLag = v, lag
		}
	}
	return best, bestLag
}

func tone(freqs []float64, n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		var v float64
		for _, f := range freqs {
			v += 5000 * math.Sin(2*math.Pi*f*float64(i)/16000)
		}
		pcm[i] = int16(v)
	}
	return pcm
}

func TestRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		freqs []float64
		min   float64
	}{
		{"low band 440+1000 Hz", []float64{440, 1000}, 20},
		{"wideband 300+2500+5500 Hz", []float64{300, 2500, 5500}, 12},
	} {
		in := tone(tc.freqs, 16000)
		enc, dec := NewEncoder(), NewDecoder()
		// feed in 20 ms chunks like RTP does
		var out []int16
		for i := 0; i < len(in); i += 320 {
			code := enc.Encode(nil, in[i:i+320])
			if len(code) != 160 {
				t.Fatalf("20 ms should be 160 bytes, got %d", len(code))
			}
			out = dec.Decode(out, code)
		}
		s, lag := snr(in, out)
		t.Logf("%s: SNR %.1f dB at delay %d samples", tc.name, s, lag)
		if s < tc.min {
			t.Errorf("%s: SNR %.1f dB < %.0f dB", tc.name, s, tc.min)
		}
	}
}

func TestSilenceStaysQuiet(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder()
	out := dec.Decode(nil, enc.Encode(nil, make([]int16, 3200)))
	var peak int16
	for _, v := range out {
		if v > peak {
			peak = v
		} else if -v > peak {
			peak = -v
		}
	}
	if peak > 64 {
		t.Fatalf("silence decodes with peak %d", peak)
	}
}
