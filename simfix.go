// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/damonto/wwan-go/sim"
	simcard "github.com/damonto/wwan-go/sim/card"
	"github.com/damonto/wwan-go/sim/simfile"
)

// lenientSIM wraps wwan-go's QCOM SIM reader and repairs EF_DIR records whose
// outer length is too short.
//
// Seen on a Telekom DE SIM (2026-11-24):
//
//	61 12 | 4F 10 A0000000871002FF4994208903100000 | 50 00
//
// The record says 0x12 = 18 bytes but holds 20: the empty label TLV (50 00) was
// not counted. Phones accept it; wwan-go's strict parser rejects the record
// ("parsing EF_DIR record 1: malformed APDU response") and no USIM is found.
type lenientSIM struct {
	*sim.QCOM
}

var efDirPath = []byte{0x2F, 0x00}

func isEFDir(f simcard.FileRef) bool {
	return len(f.AID) == 0 && bytes.HasSuffix(f.Path, efDirPath)
}

// fixEFDirRecord corrects the outer length of an application template (tag 61)
// when the inner TLVs that follow it run past the declared length.
func fixEFDirRecord(rec []byte) []byte {
	if len(rec) < 4 || rec[0] != 0x61 || rec[1] >= 0x80 {
		return rec
	}
	declared := int(rec[1])
	i := 2
	for i+2 <= len(rec) && rec[i] != 0xFF {
		l := int(rec[i+1])
		if rec[i+1] >= 0x80 || i+2+l > len(rec) {
			return rec // not simple short-form TLVs: leave it alone
		}
		i += 2 + l
	}
	actual := i - 2
	if actual <= declared || actual > 0x7F {
		return rec
	}
	out := slices.Clone(rec)
	out[1] = byte(actual)
	return out
}

func (r lenientSIM) ReadRecord(ctx context.Context, req simcard.RecordRead) ([]byte, error) {
	return lenientReadRecord(ctx, r.QCOM, req)
}

// recordReader is the part of a SIM reader the EF_DIR repair needs; both
// *sim.QCOM (VoWiFi) and *ims.QCOMClient (VoLTE) have it.
type recordReader interface {
	ReadRecord(ctx context.Context, req simcard.RecordRead) ([]byte, error)
	FileAttributes(ctx context.Context, f simcard.FileRef) (simcard.FileAttributes, error)
}

func lenientReadRecord(ctx context.Context, base recordReader, req simcard.RecordRead) ([]byte, error) {
	b, err := base.ReadRecord(ctx, req)
	if err == nil && isEFDir(req.File) {
		b = fixEFDirRecord(b)
	}
	return b, err
}

// ListApplications is QCOM.ListApplications, reading the records through the
// repairing ReadRecord above.
func (r lenientSIM) ListApplications(ctx context.Context) ([]simcard.Application, error) {
	return lenientListApplications(ctx, r)
}

// lenientListApplications reads EF_DIR through r's (repairing) ReadRecord.
func lenientListApplications(ctx context.Context, r recordReader) ([]simcard.Application, error) {
	ef := simcard.FileRef{Path: efDirPath}
	attrs, err := r.FileAttributes(ctx, ef)
	if err != nil {
		return nil, fmt.Errorf("reading EF_DIR: %w", err)
	}
	if attrs.FileStructure != simfile.StructureLinearFixed {
		return nil, errors.New("reading EF_DIR: unexpected file structure")
	}
	apps := make([]simcard.Application, 0, attrs.RecordCount)
	for id := uint16(1); id <= attrs.RecordCount; id++ {
		record, err := r.ReadRecord(ctx, simcard.RecordRead{File: ef, Record: id, Length: attrs.RecordSize})
		if err != nil {
			return nil, fmt.Errorf("reading EF_DIR record %d: %w", id, err)
		}
		var parsed simfile.EFDirRecord
		if err := parsed.UnmarshalBinary(record); err != nil {
			return nil, fmt.Errorf("parsing EF_DIR record %d: %w", id, err)
		}
		if len(parsed.AID) == 0 {
			continue
		}
		apps = append(apps, simcard.Application{AID: slices.Clone(parsed.AID), Label: parsed.Label})
	}
	return apps, nil
}
