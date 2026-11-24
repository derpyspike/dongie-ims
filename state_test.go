// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"testing"

	ims "github.com/damonto/ims-go"
	imsvoice "github.com/damonto/ims-go/ims/voice"
)

func TestMaskNumber(t *testing.T) {
	if got := maskNumber("+34600000099"); got != "+34•••••99" {
		t.Fatalf("got %q", got)
	}
	if got := maskNumber("123"); got != "•••" {
		t.Fatalf("got %q", got)
	}
}

func TestCallResults(t *testing.T) {
	s := newState("ring")
	add := func(callID string) {
		s.nextCall++
		s.calls = append(s.calls, &Call{ID: s.nextCall, callID: callID, Dir: "in", Result: "ringing"})
	}
	add("a")
	add("b")
	s.callEvent(ims.CallEvent{CallID: "a", State: imsvoice.CallStateActive})
	if e := s.callEvent(ims.CallEvent{CallID: "a", State: imsvoice.CallStateEnded}); e == nil || e.Result != "answered" {
		t.Fatalf("a: %+v", e)
	}
	if e := s.callEvent(ims.CallEvent{CallID: "b", State: imsvoice.CallStateEnded, Cause: "remote cancel"}); e == nil || e.Result != "missed" {
		t.Fatalf("b: %+v", e)
	}
	if e := s.callEvent(ims.CallEvent{CallID: "b", State: imsvoice.CallStateEnded}); e != nil {
		t.Fatalf("ended twice: %+v", e)
	}
	for i := 0; i < maxSMS+5; i++ {
		s.smsIncoming(ims.SMS{From: "x", Text: "y"})
	}
	if n := len(s.smsAfter(0)); n != maxSMS {
		t.Fatalf("sms cap: %d", n)
	}
	if n := len(s.smsAfter(uint64(maxSMS))); n != 5 {
		t.Fatalf("after: %d", n)
	}
}
