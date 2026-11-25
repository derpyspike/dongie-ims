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
	"strings"
	"syscall"
	"time"

	ims "github.com/damonto/ims-go"
	"github.com/damonto/wwan-go/qcom"
	"github.com/damonto/wwan-go/qcom/qmi"
	"github.com/damonto/wwan-go/sim"
)

//go:embed carriers.json
var defaultCarriers []byte

var version = "0.2.0"

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
	srv, err := serveAPI(*socket, *group, st, br, log)
	if err != nil {
		log.Error("api socket", "err", err)
		os.Exit(1)
	}
	defer srv.Close()
	log.Info("dongie-ims started", "version", version, "socket", *socket, "on_call", *onCall)

	backoff := 15 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := runSession(ctx, st, br, guard, sessionConfig{qmiDev: *qmiDev, imei: *imei, profiles: &profiles, log: log, imsLog: imsLog})
		if ctx.Err() != nil {
			break
		}
		st.setDown(err)
		log.Warn("IMS session ended", "err", err, "retry_in", backoff)
		if time.Since(started) > 10*time.Minute {
			backoff = 15 * time.Second
		}
		if err != nil && strings.Contains(err.Error(), "loading USIM") {
			// the SIM's files can't be read the way ims-go expects: don't keep
			// poking the modem's SIM interface every few seconds
			backoff = time.Hour
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	st.setDown(errors.New("stopped"))
	log.Info("dongie-ims stopped")
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
func runSession(ctx context.Context, st *state, br *bridge, guard *simGuard, c sessionConfig) error {
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
	tr, err := qmi.Open(ctx, qmi.WithProxy(c.qmiDev))
	if err != nil {
		return fmt.Errorf("opening QMI: %w", err)
	}
	qc, err := qcom.NewClient(tr, qcom.WithSlot(1))
	if err != nil {
		_ = tr.Close()
		return fmt.Errorf("QMI client: %w", err)
	}
	qcomReader, err := sim.NewQCOM(qc)
	if err != nil {
		_ = qc.Close()
		return fmt.Errorf("SIM: %w", err)
	}
	reader := lenientSIM{qcomReader}
	client, err := ims.New(reader, &ims.Config{
		Carriers: c.profiles,
		Terminal: ims.TerminalInfo{ID: imei},
		Logger:   c.imsLog,
		Access:   ims.VoWiFi(ims.VoWiFiConfig{}),
	})
	if err != nil {
		_ = reader.Close()
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
		_ = reader.Close()
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
