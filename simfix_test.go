// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/damonto/wwan-go/sim/simfile"
)

func TestFixEFDirRecord(t *testing.T) {
	// real EF_DIR record 1 of a Telekom DE SIM: declared 0x12, holds 0x14
	telekom, _ := hex.DecodeString("61124f10a0000000871002ff49942089031000005000")
	var strict simfile.EFDirRecord
	if err := strict.UnmarshalBinary(telekom); err == nil {
		t.Fatal("expected wwan-go to reject the raw record")
	}
	fixed := fixEFDirRecord(telekom)
	if fixed[1] != 0x14 {
		t.Fatalf("length not fixed: %x", fixed[1])
	}
	var rec simfile.EFDirRecord
	if err := rec.UnmarshalBinary(fixed); err != nil {
		t.Fatalf("fixed record rejected: %v", err)
	}
	wantAID, _ := hex.DecodeString("a0000000871002ff4994208903100000")
	if !bytes.Equal(rec.AID, wantAID) {
		t.Fatalf("AID %x", rec.AID)
	}
	// correct records and empty records stay untouched
	good, _ := hex.DecodeString("61144f10a0000000871002ff49942089031000005000ffff")
	if !bytes.Equal(fixEFDirRecord(good), good) {
		t.Fatal("touched a correct record")
	}
	empty := bytes.Repeat([]byte{0xFF}, 22)
	if !bytes.Equal(fixEFDirRecord(empty), empty) {
		t.Fatal("touched an empty record")
	}
}
