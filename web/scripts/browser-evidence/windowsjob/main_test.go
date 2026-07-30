package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseSinglePathOptionRequiresOneCanonicalAbsolutePath(t *testing.T) {
	t.Parallel()
	validPath := filepath.Join(t.TempDir(), "status.json")
	noncanonicalPath := validPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "status.json"
	tests := []struct {
		name      string
		arguments []string
		want      string
		wantError bool
	}{
		{name: "valid", arguments: []string{"--status", validPath}, want: validPath},
		{name: "missing", arguments: nil, wantError: true},
		{name: "wrong option", arguments: []string{"--other", validPath}, wantError: true},
		{name: "empty", arguments: []string{"--status", ""}, wantError: true},
		{name: "relative", arguments: []string{"--status", "status.json"}, wantError: true},
		{name: "noncanonical", arguments: []string{"--status", noncanonicalPath}, wantError: true},
		{name: "extra", arguments: []string{"--status", validPath, "unexpected"}, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSinglePathOption(test.arguments, "--status")
			if test.wantError {
				if err == nil {
					t.Fatalf("parseSinglePathOption(%q) unexpectedly succeeded with %q", test.arguments, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseSinglePathOption(%q) = %q, %v; want %q", test.arguments, got, err, test.want)
			}
		})
	}
}

func TestParseLauncherHandlesRejectsSentinelsAndMalformedOptions(t *testing.T) {
	t.Parallel()
	maximumHandle := strconv.FormatUint(uint64(^uintptr(0)), 10)
	tests := []struct {
		name      string
		arguments []string
		wantEvent uintptr
		wantStdin uintptr
		wantError bool
	}{
		{name: "valid", arguments: []string{"--event-handle", "1"}, wantEvent: 1},
		{name: "valid stdin", arguments: []string{"--event-handle", "1", "--stdin-handle", "2"}, wantEvent: 1, wantStdin: 2},
		{name: "missing", arguments: nil, wantError: true},
		{name: "wrong option", arguments: []string{"--status", "1"}, wantError: true},
		{name: "extra", arguments: []string{"--event-handle", "1", "unexpected"}, wantError: true},
		{name: "zero", arguments: []string{"--event-handle", "0"}, wantError: true},
		{name: "negative", arguments: []string{"--event-handle", "-1"}, wantError: true},
		{name: "nonnumeric", arguments: []string{"--event-handle", "handle"}, wantError: true},
		{name: "invalid sentinel", arguments: []string{"--event-handle", maximumHandle}, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotEvent, gotStdin, err := parseLauncherHandles(test.arguments)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseLauncherHandles(%q) unexpectedly succeeded with %d/%d", test.arguments, gotEvent, gotStdin)
				}
				return
			}
			if err != nil || gotEvent != test.wantEvent || gotStdin != test.wantStdin {
				t.Fatalf("parseLauncherHandles(%q) = %d/%d, %v; want %d/%d", test.arguments, gotEvent, gotStdin, err, test.wantEvent, test.wantStdin)
			}
		})
	}
}

func TestParseHandleAcceptsLargestNonSentinelValue(t *testing.T) {
	t.Parallel()
	want := ^uintptr(0) - 1
	got, err := parseHandle(strconv.FormatUint(uint64(want), 10), "test")
	if err != nil || got != want {
		t.Fatalf("parseHandle = %d, %v; want %d", got, err, want)
	}
}

func TestRunCommandRejectsInvalidDispatchAndFramesBeforePlatformWork(t *testing.T) {
	t.Parallel()
	zeroLengthFrame := []byte{0, 0, 0, 0}
	tests := []struct {
		name      string
		arguments []string
		input     []byte
	}{
		{name: "missing command"},
		{name: "unknown command", arguments: []string{"unknown"}},
		{name: "supervise option", arguments: []string{commandSupervise, "--wrong", "path"}},
		{name: "launcher option", arguments: []string{commandLauncher, "--wrong", "1"}},
		{name: "supervise missing request", arguments: []string{commandSupervise, "--status", filepath.Join(t.TempDir(), "status.json"), "--request", filepath.Join(t.TempDir(), "missing.bin")}},
		{name: "launcher zero frame", arguments: []string{commandLauncher, "--event-handle", "1"}, input: zeroLengthFrame},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := runCommand(test.arguments, bytes.NewReader(test.input)); err == nil {
				t.Fatalf("runCommand(%q) accepted invalid dispatch or frame", test.arguments)
			}
		})
	}
}
