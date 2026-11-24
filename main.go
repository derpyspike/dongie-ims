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

var version = "0.1.0"

func main() {
	var (
		socket   = flag.String("socket", "/run/dongie-ims/api.sock", "unix socket for the JSON API")
		group    = flag.String("group", "dongie", "group allowed to use the socket")
		qmiDev   = flag.String("qmi", "/dev/wwan0qmi0", "QMI device (opened through qmi-proxy)")
		carriers = flag.String("carriers", "", "carrier profile JSON (default: built-in Orange ES profile)")
		imei     = flag.String("imei", "", "terminal IMEI (default: ask ModemManager)")
		onCall   = flag.String("on-call", "ring", "incoming calls: ring (let it ring, network decides) or reject (486 Busy)")
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
		err := runSession(ctx, st, br, sessionConfig{qmiDev: *qmiDev, imei: *imei, profiles: &profiles, log: log, imsLog: imsLog})
		if ctx.Err() != nil {
			break
		}
		st.setDown(err)
		log.Warn("IMS session ended", "err", err, "retry_in", backoff)
		if time.Since(started) > 10*time.Minute {
			backoff = 15 * time.Second
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
func runSession(ctx context.Context, st *state, br *bridge, c sessionConfig) error {
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
	reader, err := sim.NewQCOM(qc)
	if err != nil {
		_ = qc.Close()
		return fmt.Errorf("SIM: %w", err)
	}
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
