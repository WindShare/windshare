//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestLinuxDispatcherUsesUnifiedEngine(t *testing.T) {
	sentinel := errors.New("unified")
	previous := runLinux
	runLinux = func([]string) error { return sentinel }
	t.Cleanup(func() {
		runLinux = previous
	})
	if err := runPlatform([]string{commandSupervise}); !errors.Is(err, sentinel) {
		t.Fatalf("unified dispatch error = %v", err)
	}
}
