// Package powerinhibit prevents idle OS system sleep during active transfers
// while leaving display turn-off and screen lock untouched.
package powerinhibit

import (
	"strings"
	"sync"
)

const defaultInhibitReason = "WindShare active transfer"

// Inhibitor acquires and releases OS-level system-sleep assertions.
type Inhibitor struct {
	acquire func(reason string) (func(), error)
}

// New returns the host platform's system-sleep inhibitor.
func New() *Inhibitor {
	return newWithAcquire(acquireSystemSleepInhibition)
}

func newWithAcquire(acquire func(reason string) (func(), error)) *Inhibitor {
	return &Inhibitor{acquire: acquire}
}

// InhibitSleep requests that the OS stay awake while active transfer work runs.
// The returned release function is always non-nil and safe to invoke multiple times.
func (i *Inhibitor) InhibitSleep(reason string) (func(), error) {
	if i == nil || i.acquire == nil {
		return func() {}, nil
	}
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		trimmed = defaultInhibitReason
	}
	release, err := i.acquire(trimmed)
	if err != nil {
		return func() {}, err
	}
	if release == nil {
		return func() {}, nil
	}
	var once sync.Once
	return func() {
		once.Do(release)
	}, nil
}
