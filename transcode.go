// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"fmt"
	"strconv"
	"strings"

	imsvoice "github.com/damonto/ims-go/ims/voice"
	"github.com/damonto/ims-go/ims/voice/codec/amr/amrnb"
	"github.com/damonto/ims-go/ims/voice/codec/amr/amrwb"
)

// frame16k is 20 ms of 16 kHz mono PCM, the common format between the IMS
// side (AMR-WB 16 kHz / AMR 8 kHz) and the browser side (G.722, 16 kHz).
const frame16k = 320

// imsCodec converts between RTP payloads of the negotiated IMS codec and
// 20 ms frames of 16 kHz PCM.
type imsCodec struct {
	media   imsvoice.NegotiatedMedia
	payload imsvoice.AMRPayloadCodec
	wb      bool
	mode    int
	encWB   *amrwb.Encoder
	decWB   *amrwb.Decoder
	encNB   *amrnb.Encoder
	decNB   *amrnb.Decoder
	nbHist  int16 // last input sample for the 16k->8k filter
	nbLast  int16 // last decoded 8 kHz sample for upsampling
}

// highestMode picks the highest mode in an SDP mode-set ("0,1,2"), capped.
func highestMode(modeSet string, def, max int) int {
	best := -1
	for _, f := range strings.Split(modeSet, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && v <= max && v > best {
			best = v
		}
	}
	if best < 0 {
		return def
	}
	return best
}

func newIMSCodec(m imsvoice.NegotiatedMedia) (*imsCodec, error) {
	c := &imsCodec{media: m, payload: imsvoice.AMRPayloadCodec{Media: m}}
	var err error
	switch m.Codec {
	case imsvoice.CodecAMRWB:
		c.wb = true
		c.mode = highestMode(m.ModeSet, 2, 8)
		if c.encWB, err = amrwb.NewEncoder(amrwb.Config{Mode: amrwb.Mode(c.mode)}); err != nil {
			return nil, err
		}
		c.decWB = amrwb.NewDecoder()
	case imsvoice.CodecAMR:
		c.mode = highestMode(m.ModeSet, 7, 7)
		if c.encNB, err = amrnb.NewEncoder(amrnb.Config{Mode: amrnb.Mode(c.mode)}); err != nil {
			return nil, err
		}
		c.decNB = amrnb.NewDecoder()
	default:
		return nil, fmt.Errorf("codec %s not supported by the browser bridge", m.Codec)
	}
	return c, nil
}

// clock is the RTP clock of the IMS codec; samplesPerFrame its 20 ms step.
func (c *imsCodec) clock() int {
	if c.wb {
		return 16000
	}
	return 8000
}

func (c *imsCodec) name() string {
	return fmt.Sprintf("%s mode %d", c.media.Codec, c.mode)
}

// decode turns one IMS RTP payload into 0..n 16 kHz frames.
func (c *imsCodec) decode(payload []byte) ([][]int16, error) {
	p, err := c.payload.Unmarshal(payload)
	if err != nil {
		return nil, err
	}
	var out [][]int16
	for _, f := range p.Frames {
		if c.wb {
			pcm, err := c.decWB.Decode(make([]int16, 0, frame16k), amrwb.Frame{Mode: amrwb.Mode(f.FrameType), Quality: f.Quality, Data: f.Data})
			if err != nil {
				return out, err
			}
			out = append(out, pcm)
			continue
		}
		pcm8, err := c.decNB.Decode(make([]int16, 0, frame16k/2), amrnb.Frame{Mode: amrnb.Mode(f.FrameType), Quality: f.Quality, Data: f.Data})
		if err != nil {
			return out, err
		}
		out = append(out, c.up(pcm8))
	}
	return out, nil
}

// encode turns one 16 kHz frame into an IMS RTP payload (CMR 15 = no request).
func (c *imsCodec) encode(pcm []int16) ([]byte, error) {
	var f imsvoice.AMRFrame
	if c.wb {
		fr, err := c.encWB.Encode(nil, pcm)
		if err != nil {
			return nil, err
		}
		f = imsvoice.AMRFrame{FrameType: int(fr.Mode), Quality: true, Data: fr.Data}
	} else {
		fr, err := c.encNB.Encode(nil, c.down(pcm))
		if err != nil {
			return nil, err
		}
		f = imsvoice.AMRFrame{FrameType: int(fr.Mode), Quality: true, Data: fr.Data}
	}
	return c.payload.Marshal(imsvoice.AMRPayload{CMR: 15, Frames: []imsvoice.AMRFrame{f}})
}

// down: 16 kHz -> 8 kHz with a [1 2 1]/4 low-pass before dropping samples.
func (c *imsCodec) down(in []int16) []int16 {
	out := make([]int16, len(in)/2)
	prev := int32(c.nbHist)
	for i := range out {
		a, b := int32(in[2*i]), int32(in[2*i+1])
		out[i] = int16((prev + 2*a + b) / 4)
		prev = b
	}
	if len(in) > 0 {
		c.nbHist = in[len(in)-1]
	}
	return out
}

// up: 8 kHz -> 16 kHz by linear interpolation.
func (c *imsCodec) up(in []int16) []int16 {
	out := make([]int16, 0, 2*len(in))
	prev := int32(c.nbLast)
	for _, s := range in {
		v := int32(s)
		out = append(out, int16((prev+v)/2), s)
		prev = v
	}
	if len(in) > 0 {
		c.nbLast = in[len(in)-1]
	}
	return out
}
