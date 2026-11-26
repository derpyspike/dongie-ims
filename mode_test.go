// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ims-mode")
	if got := readMode(p); got != defaultMode {
		t.Fatalf("missing file: %q", got)
	}
	for _, m := range []string{"volte", "vowifi", "volte+vowifi", "vowifi+volte"} {
		_ = os.WriteFile(p, []byte(m+"\n"), 0o644)
		if got := readMode(p); got != m {
			t.Fatalf("%q -> %q", m, got)
		}
	}
	_ = os.WriteFile(p, []byte("off"), 0o644)
	if got := readMode(p); got != defaultMode {
		t.Fatalf("invalid mode -> %q", got)
	}
	if o := modeOrder["volte+vowifi"]; len(o) != 2 || o[0] != accessVoLTE || o[1] != accessVoWiFi {
		t.Fatalf("order %v", o)
	}
}

func TestQMIValue(t *testing.T) {
	// qmicli --nas-get-serving-system, trimmed (UZ801 2026-11-26; cell values made up)
	out := "[/dev/wwan0qmi0] Successfully got serving system:\n\tRegistration state: 'registered'\n" +
		"\t3GPP cell ID: '12345678'\n\tLTE tracking area code: '4321'\n\tActive Channel:    '1849'\n"
	if v := qmiValue(out, "3GPP cell ID"); v != "12345678" {
		t.Fatalf("cell id %q", v)
	}
	if v := qmiValue(out, "LTE tracking area code"); v != "4321" {
		t.Fatalf("tac %q", v)
	}
	if v := qmiValue(out, "Active Channel"); v != "1849" {
		t.Fatalf("channel %q", v)
	}
	if v := qmiValue(out, "Nope"); v != "" {
		t.Fatalf("missing %q", v)
	}
}
