// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// simGuard stops a SIM from crash-looping the stick.
//
// Seen 2026-11-24 with a Telekom DE SIM on the UZ801: reading it through the
// modem's QMI UIM interface hung the 2015 modem firmware and the whole SoC reset
// (no kernel log, like a power cut), again on every boot. Before touching a SIM
// we write an "in flight" marker (fsync'd); a clean finish removes it. If a
// marker is still there at the next start, the previous attempt died mid-way:
// that SIM gets disabled for good (until the line is removed from disabled-sims).
type simGuard struct {
	dir    string
	manual []string
	log    *slog.Logger
}

func newSIMGuard(dir, manual string, log *slog.Logger) *simGuard {
	g := &simGuard{dir: dir, log: log}
	for _, c := range strings.Split(manual, ",") {
		if c = strings.TrimSpace(c); c != "" {
			g.manual = append(g.manual, c)
		}
	}
	return g
}

func (g *simGuard) marker(code string) string { return filepath.Join(g.dir, "inflight-"+code) }
func (g *simGuard) list() string              { return filepath.Join(g.dir, "disabled-sims") }

func (g *simGuard) disabledCodes() []string {
	b, _ := os.ReadFile(g.list())
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 && !strings.HasPrefix(f[0], "#") {
			out = append(out, f[0])
		}
	}
	return out
}

// blocked returns why this SIM must not be touched, or "".
func (g *simGuard) blocked(code string) string {
	if slices.Contains(g.manual, code) {
		return fmt.Sprintf("SIM %s disabled with -disable-sims", code)
	}
	if _, err := os.Stat(g.marker(code)); err == nil {
		// the last attempt with this SIM never finished: the stick died mid-way
		line := fmt.Sprintf("%s crashed-during-sim-access %s\n", code, time.Now().UTC().Format(time.RFC3339))
		if err := appendSync(g.list(), line); err != nil {
			g.log.Error("SIM guard: can't record disabled SIM", "err", err)
		}
		_ = os.Remove(g.marker(code))
		syncDir(g.dir)
		g.log.Error("SIM guard: previous SIM access never finished (stick reset?), disabling this SIM", "sim", code)
	}
	if slices.Contains(g.disabledCodes(), code) {
		return fmt.Sprintf("SIM %s disabled: an earlier attempt to read it crashed the stick (remove it from %s to retry)", code, g.list())
	}
	return ""
}

func (g *simGuard) begin(code string) error {
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(g.marker(code))
	if err != nil {
		return err
	}
	_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	syncDir(g.dir)
	return nil
}

func (g *simGuard) end(code string) {
	if err := os.Remove(g.marker(code)); err == nil {
		syncDir(g.dir)
	}
}

func appendSync(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return f.Sync()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
