//go:build windows

package windowsjob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

func TestParentWatcherJoinsBeforeClosingRetainedHandle(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	results, closeAuthority := watchRetainedParent(windows.Handle(reader.Fd()))
	started := time.Now()
	closeAuthority()
	closeAuthority()
	if elapsed := time.Since(started); elapsed > 10*jobPollInterval {
		t.Fatalf("parent watcher cancellation took %s", elapsed)
	}
	if err := errors.Join(reader.Close(), writer.Close()); err != nil {
		t.Fatal(err)
	}
	select {
	case result, ok := <-results:
		if ok {
			t.Fatalf("canceled parent watcher emitted %#v", result)
		}
	default:
	}
}

func TestParentWatcherAuthenticatesPipeClosure(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	results, closeAuthority := watchRetainedParent(windows.Handle(reader.Fd()))
	defer closeAuthority()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result, ok := <-results:
		if !ok {
			t.Fatal("parent watcher closed without reporting parent loss")
		}
		if result.err != nil || result.reason != ownerprotocol.TerminationParentLost {
			t.Fatalf("parent closure result = %#v", result)
		}
	case <-time.After(10 * jobPollInterval):
		t.Fatal("parent watcher did not observe liveness-pipe closure")
	}
}

func TestWindowsControlAuthoritiesRejectInvalidEndpoints(t *testing.T) {
	if _, _, err := watchParentProcess(supervisionRequest{}); err == nil {
		t.Fatal("missing parent liveness endpoint was accepted")
	}
	if _, err := openConfiguredPipe(0, "\x00", windows.GENERIC_READ, "control"); err == nil {
		t.Fatal("invalid control pipe path was accepted")
	}
	if err := validatePipeHandle(windows.InvalidHandle, "control"); err == nil {
		t.Fatal("invalid control pipe handle was accepted")
	}
	file, err := os.CreateTemp(t.TempDir(), "not-a-pipe-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := validatePipeHandle(windows.Handle(file.Fd()), "control"); err == nil {
		t.Fatal("non-pipe control handle was accepted")
	}
}

func TestSimultaneousControlAndParentEOFAlwaysReportsParentLoss(t *testing.T) {
	request := validSupervisionRequest(t)
	for run := range 100 {
		parentReader, parentWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		controlReader, controlWriter, err := os.Pipe()
		if err != nil {
			_ = parentReader.Close()
			_ = parentWriter.Close()
			t.Fatal(err)
		}
		parentResults, closeParent := watchRetainedParent(windows.Handle(parentReader.Fd()))
		controlResults, closeControl := watchTerminationControl(controlReader, request)
		results, closeMerge := mergeControlAuthorities(parentResults, controlResults)
		if err := errors.Join(controlWriter.Close(), parentWriter.Close()); err != nil {
			t.Fatalf("run %d: close authorities: %v", run, err)
		}
		select {
		case result := <-results:
			if result.err != nil || result.reason != ownerprotocol.TerminationParentLost {
				t.Fatalf("run %d: simultaneous EOF result = %#v", run, result)
			}
		case <-time.After(20 * jobPollInterval):
			t.Fatalf("run %d: simultaneous EOF did not settle", run)
		}
		if err := errors.Join(closeMerge(), closeControl(), closeParent(), controlReader.Close(), parentReader.Close()); err != nil {
			t.Fatalf("run %d: close watchers: %v", run, err)
		}
	}
}

func TestTargetInputPumpOwnsItsDeclaredPipeFraming(t *testing.T) {
	exact := []byte{0, 1, 2, 0xff}
	var output bytes.Buffer
	if err := streamExactTargetInput(bytes.NewReader(exact), &output, &ownerprotocol.Stdin{ByteLength: int64(len(exact))}); err != nil || !bytes.Equal(output.Bytes(), exact) {
		t.Fatalf("exact target stdin payload=%v err=%v", output.Bytes(), err)
	}
	if err := streamExactTargetInput(bytes.NewReader(exact[:2]), io.Discard,
		&ownerprotocol.Stdin{ByteLength: int64(len(exact))}); err == nil ||
		!strings.Contains(err.Error(), "read declared target stdin bytes") {
		t.Fatalf("short target stdin err=%v", err)
	}
	if err := streamExactTargetInput(bytes.NewReader(exact), io.Discard,
		&ownerprotocol.Stdin{ByteLength: int64(len(exact) - 1)}); err == nil ||
		!strings.Contains(err.Error(), "beyond its declared length") {
		t.Fatalf("overlong target stdin err=%v", err)
	}
}

func TestLauncherEventsRejectAmbiguousFraming(t *testing.T) {
	tooLong := strings.Repeat("x", maximumDiagnosticBytes+1)
	tests := []struct {
		name  string
		event launcherEvent
	}{
		{
			name: "schema",
			event: launcherEvent{
				SchemaVersion: "unsupported",
				Type:          launcherEventRootStarted, PID: 1, ProcessHandle: 1,
			},
		},
		{
			name: "root identity",
			event: launcherEvent{
				SchemaVersion: launcherEventSchema,
				Type:          launcherEventRootStarted,
			},
		},
		{
			name: "spawn identity",
			event: launcherEvent{
				SchemaVersion: launcherEventSchema,
				Type:          launcherEventSpawnFailed,
			},
		},
		{
			name: "spawn diagnostic",
			event: launcherEvent{
				SchemaVersion: launcherEventSchema,
				Type:          launcherEventSpawnFailed,
				SpawnFailure:  &tooLong,
			},
		},
		{
			name: "spawn input handle",
			event: launcherEvent{
				SchemaVersion: launcherEventSchema, Type: launcherEventSpawnFailed,
				InputHandle: 1, SpawnFailure: func() *string { value := "failed"; return &value }(),
			},
		},
		{
			name: "type",
			event: launcherEvent{
				SchemaVersion: launcherEventSchema,
				Type:          "other",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var framed bytes.Buffer
			if err := ownerprotocol.WriteFrame(&framed, test.event); err != nil {
				t.Fatal(err)
			}
			if _, err := readLauncherEvent(&framed); err == nil {
				t.Fatal("ambiguous launcher event was accepted")
			}
		})
	}
}
