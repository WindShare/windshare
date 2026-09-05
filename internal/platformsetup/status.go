// Package platformsetup reads installation-time reachability decisions without
// giving a connection attempt authority to change host policy.
package platformsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	Configured  = "configured"
	Declined    = "declined"
	Unavailable = "unavailable"
)

type Status struct {
	Schema     int    `json:"schema"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	Executable string `json:"executable"`
}

func ReadForExecutable(path, executable string) Status {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return unavailable("first-setup-not-run")
	}
	if err != nil {
		return unavailable("status-unreadable")
	}
	var status Status
	if json.Unmarshal(data, &status) != nil || status.Schema != 1 {
		return unavailable("status-invalid")
	}
	if !validDecision(status.State, status.Reason) {
		return unavailable("status-invalid")
	}
	installed := filepath.Clean(status.Executable)
	current := filepath.Clean(executable)
	equal := installed == current
	if runtime.GOOS == "windows" {
		equal = strings.EqualFold(installed, current)
	}
	if !equal {
		return unavailable("install-path-changed")
	}
	return status
}

func validDecision(state, reason string) bool {
	switch state {
	case Configured:
		return reason == "application-udp-tcp-rules-created"
	case Declined:
		return reason == "user-skipped"
	case Unavailable:
		return reason == "rule-removed" || reason == "firewall-command-unavailable-or-denied" || reason == "platform-firewall-setup-unsupported"
	default:
		return false
	}
}

func unavailable(reason string) Status { return Status{Schema: 1, State: Unavailable, Reason: reason} }
