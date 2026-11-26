// SPDX-License-Identifier: GPL-3.0-only
//
// dongie-ims keeps an OpenStick (MSM8916) registered on the carrier's IMS over
// VoWiFi (IKEv2/SWu to the ePDG, tunnelled over the normal LTE data link) using
// github.com/damonto/ims-go, and exposes a tiny JSON API on a unix socket for
// dongie: registration status, a RAM-only call log and SMS received over IMS.
//
// It runs as its own process because it needs root (modem via qmi-proxy) and is
// GPL-3.0 (ims-go), while dongie stays unprivileged and EUPL-1.2.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ims "github.com/damonto/ims-go"
	"github.com/damonto/wwan-go/qcom"
	"github.com/damonto/wwan-go/qcom/qmi"
	"github.com/damonto/wwan-go/sim"
	simcard "github.com/damonto/wwan-go/sim/card"
)

//go:embed carriers.json
var defaultCarriers []byte

var version = "0.3.0"

func main() {
	var (
		socket   = flag.String("socket", "/run/dongie-ims/api.sock", "unix socket for the JSON API")
		group    = flag.String("group", "dongie", "group allowed to use the socket")
		qmiDev   = flag.String("qmi", "/dev/wwan0qmi0", "QMI device (opened through qmi-proxy)")
		carriers = flag.String("carriers", "", "carrier profile JSON (default: built-in Orange ES profile)")
		imei     = flag.String("imei", "", "terminal IMEI (default: ask ModemManager)")
		onCall   = flag.String("on-call", "ring", "incoming calls: ring (let it ring, network decides) or reject (486 Busy)")
		stateDir = flag.String("state-dir", "/var/lib/dongie-ims", "persistent state (SIM crash guard)")
		disabled = flag.String("disable-sims", "", "comma-separated SIM MCC+MNC codes to never touch (e.g. 26201)")
		logLevel = flag.String("log-level", "info", "debug|info|warn|error (ims-go internals log at warn unless debug)")
		vmFile   = flag.String("voicemail-file", "/var/lib/dongie/ims-voicemail.json", "voicemail settings, written by dongie (missing = off)")
		greetWAV = flag.String("greeting", "/var/lib/dongie/ims-greeting.wav", "voicemail greeting: WAV, PCM 16-bit mono 16 kHz (dongie converts uploads)")
		modeFile = flag.String("mode-file", "/var/lib/dongie/ims-mode", "access mode chosen in dongie: vowifi | volte | volte+vowifi | vowifi+volte (missing = vowifi)")
		showVer  = flag.Bool("version", false, "print version")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("dongie-ims", version)
		return
	}
	if *onCall != "ring" && *onCall != "reject" {
		fmt.Fprintln(os.Stderr, "-on-call must be ring or reject")
		os.Exit(2)
	}
	lvl := parseLevel(*logLevel)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	// ims-go is chatty at info and logs phone numbers there; keep it at warn
	// unless we are explicitly debugging (journal lives on eMMC).
	imsLevel := slog.LevelWarn
	if lvl <= slog.LevelDebug {
		imsLevel = slog.LevelDebug
	}
	imsLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: imsLevel}))

	profileJSON := defaultCarriers
	if *carriers != "" {
		b, err := os.ReadFile(*carriers)
		if err != nil {
			log.Error("reading carrier profiles", "err", err)
			os.Exit(1)
		}
		profileJSON = b
	}
	var profiles ims.CarrierProfiles
	if err := json.Unmarshal(profileJSON, &profiles); err != nil {
		log.Error("parsing carrier profiles", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st := newState(*onCall)
	guard := newSIMGuard(*stateDir, *disabled, log)
	br, err := newBridge(st, log)
	if err != nil {
		log.Error("webrtc", "err", err)
		os.Exit(1)
	}
	reload := make(chan struct{}, 1)
	vm := &voicemail{st: st, log: log, configPath: *vmFile, greeting: *greetWAV, flashDir: filepath.Join(*stateDir, "voicemail")}
	srv, err := serveAPI(*socket, *group, st, br, vm, reload, log)
	if err != nil {
		log.Error("api socket", "err", err)
		os.Exit(1)
	}
	defer srv.Close()
	log.Info("dongie-ims started", "version", version, "socket", *socket, "on_call", *onCall)

	mode := readMode(*modeFile)
	idx := 0 // position in modeOrder[mode]: 0 = primary, 1 = fallback
	backoff := 15 * time.Second
	for ctx.Err() == nil {
		order := modeOrder[mode]
		if idx >= len(order) {
			idx = 0
		}
		access := order[idx]
		st.setAccess(access, mode, idx > 0)
		log.Info("IMS session", "access", access, "mode", mode, "fallback", idx > 0)

		// a mode change (dongie: POST /v1/mode/reload) or, on the fallback, a
		// periodic retry of the primary access (only while no call is up) ends
		// the session early
		sctx, cancel := context.WithCancel(ctx)
		why := make(chan string, 1)
		done := make(chan struct{})
		go func(onFallback bool) {
			var retry <-chan time.Time
			if onFallback {
				t := time.NewTicker(primaryRetry)
				defer t.Stop()
				retry = t.C
			}
			for {
				select {
				case <-done:
					return
				case <-reload:
					why <- "mode"
					cancel()
					return
				case <-retry:
					if st.activeCalls() == 0 {
						why <- "primary"
						cancel()
						return
					}
				}
			}
		}(idx > 0)
		started := time.Now()
		err := runSession(sctx, access, st, br, vm, guard, sessionConfig{qmiDev: *qmiDev, imei: *imei, profiles: &profiles, log: log, imsLog: imsLog})
		close(done)
		cancel()
		if ctx.Err() != nil {
			break
		}
		reason := ""
		select {
		case reason = <-why:
		default:
		}
		switch reason {
		case "mode":
			if m := readMode(*modeFile); m != mode {
				log.Info("mode changed", "from", mode, "to", m)
				mode = m
			}
			idx, backoff = 0, 15*time.Second
			st.setDown(errors.New("mode changed"))
			continue
		case "primary":
			log.Info("retrying the primary access", "access", order[0])
			idx = 0
			st.setDown(errors.New("retrying " + order[0]))
			continue
		}
		st.setDown(err)
		if len(order) > 1 && idx == 0 {
			// primary failed: fall back right away
			log.Warn("IMS session ended, falling back", "access", access, "err", err, "fallback", order[1])
			idx = 1
			if !sleepOrReload(ctx, 5*time.Second, reload, &mode, *modeFile, &idx) {
				break
			}
			continue
		}
		if len(order) > 1 {
			idx = 0 // fallback failed too: next round starts with the primary again
		}
		log.Warn("IMS session ended", "access", access, "err", err, "retry_in", backoff)
		if time.Since(started) > 10*time.Minute {
			backoff = 15 * time.Second
		}
		if err != nil && strings.Contains(err.Error(), "loading USIM") {
			// the SIM's files can't be read the way ims-go expects: don't keep
			// poking the modem's SIM interface every few seconds
			backoff = time.Hour
		}
		if !sleepOrReload(ctx, backoff, reload, &mode, *modeFile, &idx) {
			break
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	st.setDown(errors.New("stopped"))
	log.Info("dongie-ims stopped")
}

// primaryRetry: while on the fallback access, how often to try the primary
// again (skipped while a call is up).
const primaryRetry = 15 * time.Minute

// sleepOrReload waits d; a mode change ends the wait early (and resets to the
// new mode's primary). It returns false when ctx ended.
func sleepOrReload(ctx context.Context, d time.Duration, reload <-chan struct{}, mode *string, modeFile string, idx *int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
	case <-reload:
		*mode = readMode(modeFile)
		*idx = 0
	}
	return true
}

type sessionConfig struct {
	qmiDev   string
	imei     string
	profiles *ims.CarrierProfiles
	log      *slog.Logger
	imsLog   *slog.Logger
}

// runSession opens the modem, registers and serves events until the client
// fails for good, stays unregistered too long, or ctx is cancelled.
func runSession(ctx context.Context, access string, st *state, br *bridge, vm *voicemail, guard *simGuard, c sessionConfig) error {
	// Which SIM is in? Asked from ModemManager, which doesn't touch the SIM's
	// file system the way ims-go does.
	code, err := simCode(ctx)
	if err != nil {
		return fmt.Errorf("SIM: %w", err)
	}
	st.setSIM(code)
	if reason := guard.blocked(code); reason != "" {
		st.setStatus("disabled")
		st.noteError(errors.New(reason))
		c.log.Warn("not touching this SIM", "sim", code, "reason", reason)
		return waitSIMChange(ctx, code)
	}
	if err := guard.begin(code); err != nil {
		return fmt.Errorf("SIM guard: %w", err)
	}

	// a clean return (error or not) means the SIM access didn't kill the stick;
	// only a reset mid-way leaves the marker behind
	defer guard.end(code)

	imei := c.imei
	if imei == "" {
		var err error
		if imei, err = modemIMEI(ctx); err != nil {
			return fmt.Errorf("IMEI: %w", err)
		}
	}
	st.setStatus("connecting")
	var (
		reader      simcard.Reader
		accessCfg   ims.AccessConfig
		closeAccess func()
	)
	switch access {
	case accessVoLTE:
		reader, accessCfg, closeAccess, err = openVoLTE(ctx, st, c)
		if err != nil {
			return fmt.Errorf("VoLTE: %w", err)
		}
	default:
		reader, closeAccess, err = openVoWiFi(ctx, c)
		if err != nil {
			return err
		}
		accessCfg = ims.VoWiFi(ims.VoWiFiConfig{})
	}
	client, err := ims.New(reader, &ims.Config{
		Carriers: c.profiles,
		Terminal: ims.TerminalInfo{ID: imei},
		Logger:   c.imsLog,
		Access:   accessCfg,
	})
	if err != nil {
		closeAccess()
		return fmt.Errorf("IMS client: %w", err)
	}
	st.setClient(client)
	defer func() {
		st.setClient(nil)
		if s := br.current(); s != nil {
			s.close("IMS session ended")
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { _ = client.Close(); close(done) }()
		select {
		case <-done:
		case <-cctx.Done():
			c.log.Warn("IMS close timed out")
		}
		closeAccess()
	}()

	events := client.Events()
	defer events.Close()
	voice := client.Voice().Events()
	defer voice.Close()
	sms := client.SMS().Events()
	defer sms.Close()

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	err = client.Connect(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	guard.end(code) // SIM access works for this SIM
	if st.sync(client, "", nil) {
		snap := st.snapshot()
		c.log.Info("IMS registered", "inner_ip", snap.InnerIP, "pcscf", snap.PCSCF)
	}

	unregSince := time.Time{}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s, ok := <-events.State:
			if !ok {
				return errors.New("event stream closed")
			}
			if st.sync(client, string(s.Status), s.LastError) {
				c.log.Info("IMS registered again", "inner_ip", st.snapshot().InnerIP)
			}
			switch s.Status {
			case ims.StatusReconnecting:
				c.log.Warn("IMS reconnecting", "err", s.LastError)
			case ims.StatusFailed, ims.StatusClosed:
				return fmt.Errorf("client %s: %v", s.Status, s.LastError)
			}
		case <-events.RegistrationChanged:
			if st.sync(client, "", nil) {
				c.log.Info("IMS registered again", "inner_ip", st.snapshot().InnerIP)
			}
		case e, ok := <-events.Errors:
			if ok {
				c.log.Debug("ims-go error", "err", e)
				st.noteError(e)
			}
		case in, ok := <-voice.Incoming:
			if !ok {
				return errors.New("voice stream closed")
			}
			id := st.callIncoming(in)
			c.log.Info("incoming call", "id", id, "from", maskNumber(in.FromNumber), "policy", st.onCall)
			if st.onCall == "reject" {
				go func() {
					rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					_ = in.Call.Reject(rctx, 486, "Busy Here")
				}()
			} else {
				go vm.maybeTake(ctx, id, in) // does nothing unless voicemail is on
			}
		case ev, ok := <-voice.Events:
			if ok {
				if e := st.callEvent(ev); e != nil && e.Ended {
					c.log.Info("call ended", "id", e.ID, "result", e.Result, "cause", ev.Cause)
					go br.callEnded(e.ID)
				}
			}
		case m, ok := <-sms.Incoming:
			if ok {
				id := st.smsIncoming(m)
				c.log.Info("SMS over IMS received", "id", id, "from", maskNumber(m.From), "chars", len([]rune(m.Text)))
			}
		case <-tick.C:
			// watchdog: ims-go reconnects by itself; if it can't for 10 min,
			// tear everything down (modem/QMI may have been reset) and start over.
			st.sync(client, "", nil)
			if client.Registration().Registered {
				unregSince = time.Time{}
				continue
			}
			if unregSince.IsZero() {
				unregSince = time.Now()
			} else if time.Since(unregSince) > 10*time.Minute {
				return errors.New("not registered for 10 minutes, restarting session")
			}
		}
	}
}

// openVoWiFi: SIM access through ModemManager's QMI port (qmi-proxy); the
// SWu/IPsec tunnel itself runs in userspace over whatever internet there is.
func openVoWiFi(ctx context.Context, c sessionConfig) (simcard.Reader, func(), error) {
	tr, err := qmi.Open(ctx, qmi.WithProxy(c.qmiDev))
	if err != nil {
		return nil, nil, fmt.Errorf("opening QMI: %w", err)
	}
	qc, err := qcom.NewClient(tr, qcom.WithSlot(1))
	if err != nil {
		_ = tr.Close()
		return nil, nil, fmt.Errorf("QMI client: %w", err)
	}
	qcomReader, err := sim.NewQCOM(qc)
	if err != nil {
		_ = qc.Close()
		return nil, nil, fmt.Errorf("SIM: %w", err)
	}
	reader := lenientSIM{qcomReader}
	return reader, func() { _ = reader.Close() }, nil
}

func modemIMEI(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "mmcli", "-m", "any", "-K").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "modem.3gpp.imei" {
			if v = strings.TrimSpace(v); len(v) >= 14 && v != "--" {
				return v, nil
			}
		}
	}
	return "", errors.New("ModemManager has no IMEI yet")
}

// maskNumber keeps the first 3 and last 2 characters: +34•••••98.
func maskNumber(n string) string {
	if len(n) <= 5 {
		return "•••"
	}
	return n[:3] + "•••••" + n[len(n)-2:]
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// simCode returns the SIM's MCC+MNC from ModemManager (waits up to 2 min for
// the PIN to be entered).
func simCode(ctx context.Context) (string, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, _ := exec.CommandContext(cctx, "mmcli", "-m", "any", "-K").Output()
		cancel()
		simPath := ""
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) == "modem.generic.sim" {
				simPath = strings.TrimSpace(v)
			}
		}
		if simPath != "" && simPath != "--" {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			out, _ := exec.CommandContext(cctx, "mmcli", "-i", simPath, "-K").Output()
			cancel()
			for _, line := range strings.Split(string(out), "\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok && strings.TrimSpace(k) == "sim.properties.operator-code" {
					if v = strings.TrimSpace(v); len(v) >= 5 && v != "--" {
						return v, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("operator code not readable (no SIM / PIN not entered?)")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitSIMChange blocks until the SIM changes (a swap usually means a reboot,
// but hot-swap or a modem reset is possible) or ctx ends.
func waitSIMChange(ctx context.Context, code string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
		}
		if now, err := simCode(ctx); err == nil && now != code {
			return fmt.Errorf("SIM changed %s -> %s", code, now)
		}
	}
}
