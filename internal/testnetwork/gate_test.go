package testnetwork

import (
	"fmt"
	"testing"
)

func TestOSNetworkEnabledFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		goos             string
		runnerAuthorized bool
		want             bool
	}{
		{name: "Windows ordinary binary is gated", goos: "windows"},
		{name: "Windows verified runner is enabled", goos: "windows", runnerAuthorized: true, want: true},
		{name: "Linux retains real-stack coverage", goos: "linux", want: true},
		{name: "Linux ignores Windows authorization", goos: "linux", runnerAuthorized: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := OSNetworkEnabledFor(test.goos, test.runnerAuthorized); got != test.want {
				t.Fatalf("OSNetworkEnabledFor(%q, %v) = %v, want %v", test.goos, test.runnerAuthorized, got, test.want)
			}
		})
	}
}

func TestStableHarnessFor(t *testing.T) {
	t.Parallel()
	if !StableHarnessFor("windows", true) {
		t.Fatal("verified Windows runner was not recognized")
	}
	if StableHarnessFor("windows", false) || StableHarnessFor("linux", true) {
		t.Fatal("stable artifact identity escaped the verified Windows runner")
	}
}

func TestRequireSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		goos             string
		runnerAuthorized bool
		wantSkip         bool
	}{
		{name: "ordinary Windows", goos: "windows", wantSkip: true},
		{name: "verified Windows", goos: "windows", runnerAuthorized: true},
		{name: "Linux", goos: "linux"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			skipper := &recordingSkipper{}
			requireOSNetworkFor(skipper, test.goos, test.runnerAuthorized)
			if skipper.skipped != test.wantSkip {
				t.Fatalf("skipped = %v, want %v", skipper.skipped, test.wantSkip)
			}
			if skipper.helpers != 1 {
				t.Fatalf("Helper calls = %d, want 1", skipper.helpers)
			}
		})
	}
}

type recordingSkipper struct {
	helpers int
	skipped bool
	message string
}

func (s *recordingSkipper) Helper() {
	s.helpers++
}

func (s *recordingSkipper) Skip(args ...any) {
	s.skipped = true
	s.message = fmt.Sprint(args...)
}
