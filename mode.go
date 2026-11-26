// SPDX-License-Identifier: GPL-3.0-only

package main

// Which IMS access to use, chosen in dongie's UI. "Off" is not a mode here:
// dongie stops this service for that. The file is written by dongie
// (unprivileged) and only read here; anything unknown means the default.

import (
	"os"
	"strings"
)

const (
	accessVoWiFi = "vowifi"
	accessVoLTE  = "volte"
	defaultMode  = "vowifi"
)

// modeOrder: the accesses of a mode, primary first, fallback second.
var modeOrder = map[string][]string{
	"vowifi":       {accessVoWiFi},
	"volte":        {accessVoLTE},
	"volte+vowifi": {accessVoLTE, accessVoWiFi},
	"vowifi+volte": {accessVoWiFi, accessVoLTE},
}

func validMode(m string) bool {
	_, ok := modeOrder[m]
	return ok
}

// readMode returns the mode in path, or defaultMode if missing/invalid.
func readMode(path string) string {
	if path == "" {
		return defaultMode
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultMode
	}
	if m := strings.TrimSpace(string(b)); validMode(m) {
		return m
	}
	return defaultMode
}
