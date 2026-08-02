package windowsjob

import (
	"bytes"
	"strconv"
	"testing"
)

func TestParseSuperviseHandleRequiresCanonicalDecimal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     string
		want      uintptr
		wantError bool
	}{
		{name: "valid", value: "42", want: 42},
		{name: "empty", wantError: true},
		{name: "leading zero", value: "042", wantError: true},
		{name: "leading plus", value: "+42", wantError: true},
		{name: "zero", value: "0", wantError: true},
		{name: "negative", value: "-1", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSuperviseHandle(test.value, "status")
			if test.wantError {
				if err == nil {
					t.Fatalf("parseSuperviseHandle(%q) unexpectedly succeeded with %d", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseSuperviseHandle(%q) = %d, %v; want %d", test.value, got, err, test.want)
			}
		})
	}
}

func TestParseLauncherHandlesRejectsSentinelsAndMalformedOptions(t *testing.T) {
	t.Parallel()
	maximumHandle := strconv.FormatUint(uint64(^uintptr(0)), 10)
	tests := []struct {
		name       string
		arguments  []string
		wantEvent  uintptr
		wantTarget uintptr
		wantError  bool
	}{
		{name: "valid", arguments: []string{"--event-handle", "1"}, wantEvent: 1},
		{name: "valid target event", arguments: []string{"--event-handle", "1", "--target-event-handle", "3"}, wantEvent: 1, wantTarget: 3},
		{name: "removed stdin option", arguments: []string{"--event-handle", "1", "--stdin-handle", "2"}, wantError: true},
		{name: "missing", arguments: nil, wantError: true},
		{name: "wrong option", arguments: []string{"--status", "1"}, wantError: true},
		{name: "extra", arguments: []string{"--event-handle", "1", "unexpected"}, wantError: true},
		{name: "zero", arguments: []string{"--event-handle", "0"}, wantError: true},
		{name: "negative", arguments: []string{"--event-handle", "-1"}, wantError: true},
		{name: "nonnumeric", arguments: []string{"--event-handle", "handle"}, wantError: true},
		{name: "invalid sentinel", arguments: []string{"--event-handle", maximumHandle}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotEvent, gotTarget, err := parseLauncherHandles(test.arguments)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseLauncherHandles(%q) unexpectedly succeeded with %d/%d", test.arguments, gotEvent, gotTarget)
				}
				return
			}
			if err != nil || gotEvent != test.wantEvent || gotTarget != test.wantTarget {
				t.Fatalf("parseLauncherHandles(%q) = %d/%d, %v; want %d/%d", test.arguments, gotEvent, gotTarget, err, test.wantEvent, test.wantTarget)
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
		{name: "supervise missing endpoints", arguments: []string{commandSupervise, "--status-handle", "1"}},
		{name: "launcher zero frame", arguments: []string{commandLauncher, "--event-handle", "1"}, input: zeroLengthFrame},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := runCommand(test.arguments, bytes.NewReader(test.input)); err == nil {
				t.Fatalf("runCommand(%q) accepted invalid dispatch or frame", test.arguments)
			}
		})
	}
}
