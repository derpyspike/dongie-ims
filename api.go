// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// serveAPI listens on a unix socket (root:<group>, 0660). No TCP on purpose:
// only dongie (in <group>) and root can talk to it.
//
//	GET  /v1/status             registration snapshot
//	GET  /v1/calls              RAM call log (last 50)
//	GET  /v1/messages?after=N   SMS received over IMS with id > N (last 100 kept)
//	POST /v1/calls/{id}/reject  reject a ringing call (486 Busy Here)
//	POST /v1/calls/{id}/answer  {"sdp": browser offer} -> {"sdp": answer}; bridges audio
//	POST /v1/calls/{id}/hangup  end the browser call / cancel dialing
//	POST /v1/calls/{id}/dtmf    {"digits": "123#"}
//	POST /v1/dial               {"to": "+34…", "sdp": offer} -> {"id", "sdp"}
func serveAPI(path, group string, st *state, br *bridge, vm *voicemail, reload chan<- struct{}, log *slog.Logger) (*http.Server, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	gid := -1
	if g, err := user.LookupGroup(group); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	} else {
		log.Warn("socket group not found, socket stays root-only", "group", group)
	}
	mode := os.FileMode(0o600)
	if gid >= 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			log.Warn("chown socket", "err", err)
		} else {
			mode = 0o660
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}

	mux := http.NewServeMux()
	// dongie wrote a new mode into the mode file: switch now (ends the current
	// session, also during a call: dongie asks the user before)
	mux.HandleFunc("POST /v1/mode/reload", func(w http.ResponseWriter, r *http.Request) {
		select {
		case reload <- struct{}{}:
		default:
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
	// voicemail: list, play (WAV), delete. Settings and greeting are files dongie writes.
	mux.HandleFunc("GET /v1/voicemail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"voicemail": vm.list(), "config": readVMConfig(vm.configPath)})
	})
	mux.HandleFunc("GET /v1/voicemail/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := vm.wav(strings.TrimSuffix(r.PathValue("id"), ".wav"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such voicemail"})
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	})
	mux.HandleFunc("DELETE /v1/voicemail/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !vm.remove(r.PathValue("id")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such voicemail"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, st.snapshot())
	})
	mux.HandleFunc("GET /v1/calls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"calls": st.callList()})
	})
	mux.HandleFunc("GET /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		writeJSON(w, http.StatusOK, map[string]any{"messages": st.smsAfter(after)})
	})
	mux.HandleFunc("POST /v1/calls/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
			return
		}
		call, ok := st.liveCall(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such ringing call"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := call.Reject(ctx, 486, "Busy Here"); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("reject: %v", err)})
			return
		}
		log.Info("call rejected via API", "id", id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /v1/calls/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
		var req struct {
			SDP string `json:"sdp"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		sdp, err := br.answer(id, req.SDP)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"sdp": sdp})
	})
	mux.HandleFunc("POST /v1/calls/{id}/hangup", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if br.hangup(id) {
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		// not bridged: a ringing call gets rejected, anything else is gone already
		if call, ok := st.ringingCall(id); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			st.liveCall(id) // marks it rejected
			_ = call.Reject(ctx, 486, "Busy Here")
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such active call"})
	})
	mux.HandleFunc("POST /v1/calls/{id}/dtmf", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
		var req struct {
			Digits string `json:"digits"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if !validDTMF(req.Digits) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "digits: 0-9 * # A-D, max 32"})
			return
		}
		if err := br.dtmf(id, req.Digits); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/dial", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To  string `json:"to"`
			SDP string `json:"sdp"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		to := strings.ReplaceAll(strings.TrimSpace(req.To), " ", "")
		if !validNumber(to) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "number: optional + then 3-20 digits"})
			return
		}
		client := st.currentClient()
		if client == nil || !st.snapshot().Registered {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not registered on IMS"})
			return
		}
		id, sdp, err := br.dial(client, to, req.SDP)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		log.Info("dialing", "id", id, "to", maskNumber(to))
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "sdp": sdp})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("api server", "err", err)
		}
	}()
	return srv, nil
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON body"})
		return false
	}
	return true
}

func validNumber(n string) bool {
	d := strings.TrimPrefix(n, "+")
	if len(d) < 3 || len(d) > 20 {
		return false
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func validDTMF(d string) bool {
	if d == "" || len(d) > 32 {
		return false
	}
	return strings.Trim(strings.ToUpper(d), "0123456789*#ABCD") == ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
