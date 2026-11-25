// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestSIMGuard(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	g := newSIMGuard(dir, "99999", log)
	if r := g.blocked("99999"); !strings.Contains(r, "-disable-sims") {
		t.Fatalf("manual: %q", r)
	}
	// clean run: begin + end -> never blocked
	if err := g.begin("21401"); err != nil {
		t.Fatal(err)
	}
	g.end("21401")
	if r := g.blocked("21401"); r != "" {
		t.Fatalf("clean SIM blocked: %q", r)
	}
	// crash: begin without end, "restart" -> blocked, and stays blocked
	if err := g.begin("26201"); err != nil {
		t.Fatal(err)
	}
	g2 := newSIMGuard(dir, "", log)
	if r := g2.blocked("26201"); !strings.Contains(r, "crashed") {
		t.Fatalf("crashed SIM not blocked: %q", r)
	}
	if r := newSIMGuard(dir, "", log).blocked("26201"); r == "" {
		t.Fatal("block not persistent")
	}
	if r := g2.blocked("21401"); r != "" {
		t.Fatalf("other SIM blocked: %q", r)
	}
}
