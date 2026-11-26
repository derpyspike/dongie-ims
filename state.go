// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"net"
	"sync"
	"time"

	ims "github.com/damonto/ims-go"
	imsvoice "github.com/damonto/ims-go/ims/voice"
)

const (
	maxCalls = 50
	maxSMS   = 100
)

// Snapshot is what GET /v1/status returns.
type Snapshot struct {
	Version       string   `json:"version"`
	Access        string   `json:"access"`             // vowifi | volte: what the current session uses
	Mode          string   `json:"mode"`               // vowifi | volte | volte+vowifi | vowifi+volte
	Fallback      bool     `json:"fallback,omitempty"` // running on the mode's second access
	Status        string   `json:"status"`
	Registered    bool     `json:"registered"`
	Since         int64    `json:"since,omitempty"` // unix time of the current registration
	Started       int64    `json:"started"`
	Registrations int      `json:"registrations"` // successful registrations since start
	LastError     string   `json:"last_error,omitempty"`
	LastErrorAt   int64    `json:"last_error_at,omitempty"`
	MSISDN        string   `json:"msisdn,omitempty"`
	InnerIP       string   `json:"inner_ip,omitempty"`
	PCSCF         []string `json:"pcscf,omitempty"`
	OnCall        string   `json:"on_call"`
	SIM           string   `json:"sim,omitempty"` // MCC+MNC of the inserted SIM
	ActiveCalls   int      `json:"active_calls"`
}

type Call struct {
	ID         uint64   `json:"id"`
	callID     string   // SIP Call-ID, never exported (contains numbers)
	Dir        string   `json:"dir"`
	From       string   `json:"from"`
	At         int64    `json:"at"`
	State      string   `json:"state"`
	Ended      bool     `json:"ended"`
	EndedAt    int64    `json:"ended_at,omitempty"`
	Result     string   `json:"result"` // ringing | dialing | answered | voicemail | missed | rejected | failed
	Cause      string   `json:"cause,omitempty"`
	Codecs     []string `json:"codecs,omitempty"` // offered by the network
	Codec      string   `json:"codec,omitempty"`  // bridged codec, e.g. "AMR-WB mode 2"
	AnsweredAt int64    `json:"answered_at,omitempty"`
	Voicemail  string   `json:"voicemail,omitempty"`      // id of the recording (result "voicemail")
	VoiceSecs  int      `json:"voicemail_secs,omitempty"` // its length
	Browser    string   `json:"browser,omitempty"`        // WebRTC state of the browser leg
	answered   bool
	byVM       bool // taken by the stick's voicemail
	rejected   bool
}

type SMS struct {
	ID   uint64 `json:"id"`
	From string `json:"from"`
	Text string `json:"text"`
	At   int64  `json:"at"`
}

type state struct {
	mu       sync.Mutex
	onCall   string
	snap     Snapshot
	calls    []*Call
	live     map[uint64]*imsvoice.Call
	sms      []SMS
	nextCall uint64
	nextSMS  uint64
	client   *ims.Client
	pdnIP    string   // VoLTE IMS PDN address
	pdnPCSCF []string // VoLTE P-CSCFs (via PCO)
}

func (s *state) setClient(c *ims.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = c
}

func (s *state) currentClient() *ims.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *state) entry(id uint64) *Call {
	for _, c := range s.calls {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (s *state) addCall(c *Call) {
	s.calls = append(s.calls, c)
	if len(s.calls) > maxCalls {
		s.calls = s.calls[len(s.calls)-maxCalls:]
	}
}

// callOutgoing logs a call we are about to dial.
func (s *state) callOutgoing(to string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextCall++
	s.addCall(&Call{ID: s.nextCall, Dir: "out", From: to, At: time.Now().Unix(), State: "dialing", Result: "dialing"})
	return s.nextCall
}

func (s *state) callConnected(id uint64, call *imsvoice.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil {
		c.callID = call.ID()
		c.State, c.Result, c.answered = "active", "answered", true
		c.AnsweredAt = time.Now().Unix()
		s.live[id] = call
	}
}

func (s *state) callFailed(id uint64, cause string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil && !c.Ended {
		c.Ended, c.EndedAt, c.State, c.Result, c.Cause = true, time.Now().Unix(), "ended", "failed", cause
		delete(s.live, id)
	}
}

func (s *state) setCodec(id uint64, codec string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil {
		c.Codec = codec
	}
}

func (s *state) setBrowser(id uint64, st string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil {
		c.Browser = st
	}
}

// claimRinging returns a ringing incoming call and marks it taken, so the
// browser and the voicemail can't both answer it.
func (s *state) claimRinging(id uint64) (*imsvoice.Call, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.entry(id)
	call, ok := s.live[id]
	if c == nil || !ok || c.Ended || c.Dir != "in" || c.answered || c.rejected {
		return nil, false
	}
	c.answered = true
	return call, true
}

func (s *state) markVoicemail(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil {
		c.byVM, c.Result, c.State = true, "voicemail", "active"
		if c.AnsweredAt == 0 {
			c.AnsweredAt = time.Now().Unix()
		}
	}
}

func (s *state) setVoicemail(id uint64, vmID string, secs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.entry(id); c != nil {
		c.Voicemail, c.VoiceSecs = vmID, secs
	}
}

func (s *state) callEnded(id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.entry(id)
	return c == nil || c.Ended
}

// ringingCall returns an incoming call that can still be answered.
func (s *state) ringingCall(id uint64) (*imsvoice.Call, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.entry(id)
	call, ok := s.live[id]
	if c == nil || !ok || c.Ended || c.Dir != "in" || c.answered {
		return nil, false
	}
	return call, true
}

func newState(onCall string) *state {
	return &state{
		onCall: onCall,
		snap:   Snapshot{Version: version, Access: accessVoWiFi, Mode: defaultMode, Status: "starting", Started: time.Now().Unix(), OnCall: onCall},
		live:   map[uint64]*imsvoice.Call{},
	}
}

func (s *state) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.PCSCF = append([]string(nil), s.snap.PCSCF...)
	out.ActiveCalls = len(s.live)
	return out
}

// sync updates the snapshot from the client's live registration state (event
// payloads can be stale: buffered events are replayed after Connect). It returns
// true when we just became registered.
func (s *state) sync(c *ims.Client, status string, err error) bool {
	reg := c.Registration()
	tun := c.Tunnel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.noteErrorLocked(err)
	}
	became := reg.Registered && !s.snap.Registered
	if became {
		s.snap.Registrations++
		s.snap.Since = time.Now().Unix()
	}
	s.snap.Registered = reg.Registered
	if reg.Registered {
		s.snap.Status = string(ims.StatusRegistered)
		if reg.MSISDN != "" {
			s.snap.MSISDN = reg.MSISDN
		}
		if s.snap.Access == accessVoLTE {
			// no tunnel: the IMS PDN's address and P-CSCFs (p2pNetwork)
			s.snap.InnerIP = s.pdnIP
			s.snap.PCSCF = append([]string(nil), s.pdnPCSCF...)
		} else {
			s.snap.InnerIP = ipString(tun.InnerIPv4)
			if s.snap.InnerIP == "" && !tun.InnerIPv6.IsZero() {
				s.snap.InnerIP = tun.InnerIPv6.IP.String() // IPv6-only IMS (e.g. Telekom DE)
			}
			s.snap.PCSCF = nil
			for _, ip := range tun.PCSCFIPs {
				s.snap.PCSCF = append(s.snap.PCSCF, ip.String())
			}
		}
	} else {
		s.snap.Since = 0
		if status != "" {
			s.snap.Status = status
		}
	}
	return became
}

// setAccess records the access the next session uses.
func (s *state) setAccess(access, mode string, fallback bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Access, s.snap.Mode, s.snap.Fallback = access, mode, fallback
	s.pdnIP, s.pdnPCSCF = "", nil
}

// setPDN records the VoLTE IMS PDN (from p2pNetwork.Replace).
func (s *state) setPDN(ip string, pcscf []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pdnIP, s.pdnPCSCF = ip, pcscf
}

func (s *state) activeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

func (s *state) setSIM(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.SIM = code
}

func (s *state) setStatus(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Status = status
}

func (s *state) setDown(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Status = "down"
	s.snap.Registered = false
	s.snap.Since = 0
	s.snap.InnerIP = ""
	s.snap.PCSCF = nil
	if err != nil {
		s.noteErrorLocked(err)
	}
	s.live = map[uint64]*imsvoice.Call{}
}

func (s *state) noteError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteErrorLocked(err)
}

func (s *state) noteErrorLocked(err error) {
	s.snap.LastError = err.Error()
	s.snap.LastErrorAt = time.Now().Unix()
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (s *state) callIncoming(in ims.IncomingCall) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextCall++
	c := &Call{
		ID: s.nextCall, callID: in.Call.ID(), Dir: "in", From: in.FromNumber,
		At: in.ReceivedAt.Unix(), State: "ringing", Result: "ringing",
	}
	if c.At <= 0 {
		c.At = time.Now().Unix()
	}
	seen := map[string]bool{}
	for _, f := range in.Offer.Formats {
		name := string(f.Codec)
		if name == "telephone-event" || seen[name] {
			continue
		}
		seen[name] = true
		c.Codecs = append(c.Codecs, name)
	}
	if s.onCall == "reject" {
		c.rejected = true
	}
	s.addCall(c)
	s.live[c.ID] = in.Call
	return c.ID
}

func (s *state) callEvent(ev ims.CallEvent) *Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c *Call
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].callID == ev.CallID {
			c = s.calls[i]
			break
		}
	}
	if c == nil || c.Ended {
		return nil
	}
	c.State = string(ev.State)
	switch ev.State {
	case imsvoice.CallStateActive, imsvoice.CallStateConfirmed:
		if c.AnsweredAt == 0 {
			c.AnsweredAt = time.Now().Unix()
		}
		c.answered = true
		if !c.byVM {
			c.Result = "answered"
		}
	case imsvoice.CallStateEnded, imsvoice.CallStateFailed:
		c.Ended = true
		c.EndedAt = time.Now().Unix()
		c.Cause = ev.Cause
		switch {
		case c.byVM:
			c.Result = "voicemail"
		case c.answered:
			c.Result = "answered"
		case c.rejected:
			c.Result = "rejected"
		default:
			c.Result = "missed"
		}
		delete(s.live, c.ID)
	}
	cp := *c
	return &cp
}

// liveCall returns the ims-go call for a ringing/active call id.
func (s *state) liveCall(id uint64) (*imsvoice.Call, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.live[id]
	if ok {
		for _, e := range s.calls {
			if e.ID == id {
				e.rejected = true
			}
		}
	}
	return c, ok
}

func (s *state) callList() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, *c)
	}
	return out
}

func (s *state) smsIncoming(m ims.SMS) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSMS++
	at := m.ReceivedAt.Unix()
	if at <= 0 {
		at = time.Now().Unix()
	}
	s.sms = append(s.sms, SMS{ID: s.nextSMS, From: m.From, Text: m.Text, At: at})
	if len(s.sms) > maxSMS {
		s.sms = s.sms[len(s.sms)-maxSMS:]
	}
	return s.nextSMS
}

func (s *state) smsAfter(after uint64) []SMS {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SMS{}
	for _, m := range s.sms {
		if m.ID > after {
			out = append(out, m)
		}
	}
	return out
}
