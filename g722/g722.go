// SPDX-License-Identifier: GPL-3.0-only
//
// Package g722 is a 64 kbit/s ITU-T G.722 (SB-ADPCM, 16 kHz) encoder/decoder,
// written after the ITU-T G.722 block diagrams; the arithmetic follows the
// widely used CMU/spandsp fixed-point formulation (which browsers' G.722 also
// derive from), so bit-exactness with WebRTC peers is what we aim for.
package g722

var (
	qmfCoeffs = [12]int32{3, -11, 12, 32, -210, 951, 3876, -805, 362, -156, 53, -11}
	q6        = [32]int32{0, 35, 72, 110, 150, 190, 233, 276, 323, 370, 422, 473, 530, 587, 650, 714, 786, 858, 940, 1023, 1121, 1219, 1339, 1458, 1612, 1765, 1980, 2195, 2557, 2919, 0, 0}
	iln       = [32]int32{0, 63, 62, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 0}
	ilp       = [32]int32{0, 61, 60, 59, 58, 57, 56, 55, 54, 53, 52, 51, 50, 49, 48, 47, 46, 45, 44, 43, 42, 41, 40, 39, 38, 37, 36, 35, 34, 33, 32, 0}
	wl        = [8]int32{-60, -30, 58, 172, 334, 538, 1198, 3042}
	rl42      = [16]int32{0, 7, 6, 5, 4, 3, 2, 1, 7, 6, 5, 4, 3, 2, 1, 0}
	ilb       = [32]int32{2048, 2093, 2139, 2186, 2233, 2282, 2332, 2383, 2435, 2489, 2543, 2599, 2656, 2714, 2774, 2834, 2896, 2960, 3025, 3091, 3158, 3228, 3298, 3371, 3444, 3520, 3597, 3676, 3756, 3838, 3922, 4008}
	qm4       = [16]int32{0, -20456, -12896, -8968, -6288, -4240, -2584, -1200, 20456, 12896, 8968, 6288, 4240, 2584, 1200, 0}
	qm2       = [4]int32{-7408, -1616, 7408, 1616}
	qm6       = [64]int32{
		-136, -136, -136, -136, -24808, -21904, -19008, -16704,
		-14984, -13512, -12280, -11192, -10232, -9360, -8576, -7856,
		-7192, -6576, -6000, -5456, -4944, -4464, -4008, -3576,
		-3168, -2776, -2400, -2032, -1688, -1360, -1040, -728,
		24808, 21904, 19008, 16704, 14984, 13512, 12280, 11192,
		10232, 9360, 8576, 7856, 7192, 6576, 6000, 5456,
		4944, 4464, 4008, 3576, 3168, 2776, 2400, 2032,
		1688, 1360, 1040, 728, 432, 136, -432, -136,
	}
	ihn = [3]int32{0, 1, 0}
	ihp = [3]int32{0, 3, 2}
	wh  = [3]int32{0, -214, 798}
	rh2 = [4]int32{2, 1, 2, 1}
)

func sat(v int32) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

type band struct {
	s, sp, sz int32
	r, a, ap  [3]int32
	p         [3]int32
	d, b, bp  [7]int32
	sg        [7]int32
	nb, det   int32
}

// block4 is the adaptive predictor shared by encoder and decoder.
func (b *band) block4(d int32) {
	b.d[0] = d
	b.r[0] = sat(b.s + d)
	b.p[0] = sat(b.sz + d)
	// UPPOL2
	for i := 0; i < 3; i++ {
		b.sg[i] = b.p[i] >> 15
	}
	wd1 := sat(b.a[1] << 2)
	wd2 := wd1
	if b.sg[0] == b.sg[1] {
		wd2 = -wd1
	}
	if wd2 > 32767 {
		wd2 = 32767
	}
	wd3 := int32(-128)
	if b.sg[0] == b.sg[2] {
		wd3 = 128
	}
	wd3 += wd2 >> 7
	wd3 += (b.a[2] * 32512) >> 15
	if wd3 > 12288 {
		wd3 = 12288
	} else if wd3 < -12288 {
		wd3 = -12288
	}
	b.ap[2] = wd3
	// UPPOL1
	b.sg[0] = b.p[0] >> 15
	b.sg[1] = b.p[1] >> 15
	wd1 = -192
	if b.sg[0] == b.sg[1] {
		wd1 = 192
	}
	wd2 = (b.a[1] * 32640) >> 15
	b.ap[1] = sat(wd1 + wd2)
	wd3 = sat(15360 - b.ap[2])
	if b.ap[1] > wd3 {
		b.ap[1] = wd3
	} else if b.ap[1] < -wd3 {
		b.ap[1] = -wd3
	}
	// UPZERO
	wd1 = 128
	if d == 0 {
		wd1 = 0
	}
	b.sg[0] = d >> 15
	for i := 1; i < 7; i++ {
		b.sg[i] = b.d[i] >> 15
		wd2 = -wd1
		if b.sg[i] == b.sg[0] {
			wd2 = wd1
		}
		wd3 = (b.b[i] * 32640) >> 15
		b.bp[i] = sat(wd2 + wd3)
	}
	// DELAYZ / DELAYA
	for i := 6; i > 0; i-- {
		b.d[i] = b.d[i-1]
		b.b[i] = b.bp[i]
	}
	for i := 2; i > 0; i-- {
		b.r[i] = b.r[i-1]
		b.p[i] = b.p[i-1]
		b.a[i] = b.ap[i]
	}
	// FILTEP
	wd1 = sat(b.r[1] + b.r[1])
	wd1 = (b.a[1] * wd1) >> 15
	wd2 = sat(b.r[2] + b.r[2])
	wd2 = (b.a[2] * wd2) >> 15
	b.sp = sat(wd1 + wd2)
	// FILTEZ
	b.sz = 0
	for i := 6; i > 0; i-- {
		wd1 = sat(b.d[i] + b.d[i])
		b.sz += (b.b[i] * wd1) >> 15
	}
	b.sz = sat(b.sz)
	// PREDIC
	b.s = sat(b.sp + b.sz)
}

func scale(nb, shift int32) int32 {
	wd1 := (nb >> 6) & 31
	wd2 := shift - (nb >> 11)
	var wd3 int32
	if wd2 < 0 {
		wd3 = ilb[wd1] << -wd2
	} else {
		wd3 = ilb[wd1] >> wd2
	}
	return wd3 << 2
}

func lowLog(nb, il4 int32) int32 {
	nb = (nb*127)>>7 + wl[il4]
	if nb < 0 {
		return 0
	}
	if nb > 18432 {
		return 18432
	}
	return nb
}

func highLog(nb, ih2 int32) int32 {
	nb = (nb*127)>>7 + wh[ih2]
	if nb < 0 {
		return 0
	}
	if nb > 22528 {
		return 22528
	}
	return nb
}

// Encoder turns 16 kHz PCM into 64 kbit/s G.722 (one byte per two samples).
type Encoder struct {
	x    [24]int32
	band [2]band
}

func NewEncoder() *Encoder {
	e := &Encoder{}
	e.band[0].det = 32
	e.band[1].det = 8
	return e
}

// Encode appends len(pcm)/2 code bytes to dst. len(pcm) should be even.
func (e *Encoder) Encode(dst []byte, pcm []int16) []byte {
	for j := 0; j+1 < len(pcm); j += 2 {
		copy(e.x[:22], e.x[2:])
		e.x[22] = int32(pcm[j])
		e.x[23] = int32(pcm[j+1])
		var sumodd, sumeven int32
		for i := 0; i < 12; i++ {
			sumodd += e.x[2*i] * qmfCoeffs[i]
			sumeven += e.x[2*i+1] * qmfCoeffs[11-i]
		}
		xlow := (sumeven + sumodd) >> 14
		xhigh := (sumeven - sumodd) >> 14

		lb := &e.band[0]
		el := sat(xlow - lb.s)
		wd := el
		if el < 0 {
			wd = -(el + 1)
		}
		i := 1
		for ; i < 30; i++ {
			if wd < (q6[i]*lb.det)>>12 {
				break
			}
		}
		ilow := ilp[i]
		if el < 0 {
			ilow = iln[i]
		}
		ril := ilow >> 2
		dlow := (lb.det * qm4[ril]) >> 15
		lb.nb = lowLog(lb.nb, rl42[ril])
		lb.det = scale(lb.nb, 8)
		lb.block4(dlow)

		hb := &e.band[1]
		eh := sat(xhigh - hb.s)
		wd = eh
		if eh < 0 {
			wd = -(eh + 1)
		}
		mih := 1
		if wd >= (564*hb.det)>>12 {
			mih = 2
		}
		ihigh := ihp[mih]
		if eh < 0 {
			ihigh = ihn[mih]
		}
		dhigh := (hb.det * qm2[ihigh]) >> 15
		hb.nb = highLog(hb.nb, rh2[ihigh])
		hb.det = scale(hb.nb, 10)
		hb.block4(dhigh)

		dst = append(dst, byte(ihigh<<6|ilow))
	}
	return dst
}

// Decoder turns 64 kbit/s G.722 back into 16 kHz PCM (two samples per byte).
type Decoder struct {
	x    [24]int32
	band [2]band
}

func NewDecoder() *Decoder {
	d := &Decoder{}
	d.band[0].det = 32
	d.band[1].det = 8
	return d
}

func (d *Decoder) Decode(dst []int16, code []byte) []int16 {
	for _, c := range code {
		ilow := int32(c) & 0x3f
		ihigh := (int32(c) >> 6) & 0x03

		lb := &d.band[0]
		rlow := lb.s + (lb.det*qm6[ilow])>>15
		if rlow > 16383 {
			rlow = 16383
		} else if rlow < -16384 {
			rlow = -16384
		}
		ril := ilow >> 2
		dlow := (lb.det * qm4[ril]) >> 15
		lb.nb = lowLog(lb.nb, rl42[ril])
		lb.det = scale(lb.nb, 8)
		lb.block4(dlow)

		hb := &d.band[1]
		dhigh := (hb.det * qm2[ihigh]) >> 15
		rhigh := dhigh + hb.s
		if rhigh > 16383 {
			rhigh = 16383
		} else if rhigh < -16384 {
			rhigh = -16384
		}
		hb.nb = highLog(hb.nb, rh2[ihigh])
		hb.det = scale(hb.nb, 10)
		hb.block4(dhigh)

		copy(d.x[:22], d.x[2:])
		d.x[22] = rlow + rhigh
		d.x[23] = rlow - rhigh
		var xout1, xout2 int32
		for i := 0; i < 12; i++ {
			xout2 += d.x[2*i] * qmfCoeffs[i]
			xout1 += d.x[2*i+1] * qmfCoeffs[11-i]
		}
		dst = append(dst, int16(sat(xout1>>11)), int16(sat(xout2>>11)))
	}
	return dst
}
