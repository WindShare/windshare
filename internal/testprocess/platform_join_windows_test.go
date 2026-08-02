//go:build windows

package testprocess

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWindowsWaitStateUsesOneBoundedJoinBudget(t *testing.T) {
	state := windowsWaitState{}
	started := time.Now()
	if state.collect(
		make(chan windowsStatusResult),
		make(chan error),
		make(chan error),
		make(chan error),
		make(chan error),
		make(chan startGateResult),
		25*time.Millisecond,
	) {
		t.Fatal("stalled Windows transport reported a complete join")
	}
	elapsed := time.Since(started)
	if elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded Windows join elapsed %s", elapsed)
	}
}

func TestWindowsStartupFailureStateUsesOneBudgetForEveryTransportTask(t *testing.T) {
	state := windowsStartupJoinState{}
	started := time.Now()
	if state.collect(
		make(chan error),
		make(chan windowsStatusResult),
		make(chan error),
		make(chan error),
		make(chan error),
		25*time.Millisecond,
	) {
		t.Fatal("stalled Windows startup transport reported a complete join")
	}
	elapsed := time.Since(started)
	if elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded Windows startup join elapsed %s", elapsed)
	}
	if pending := strings.Join(state.pending(), ","); pending != "readiness,settlement,input,stderr,process wait" {
		t.Fatalf("pending Windows startup tasks = %q", pending)
	}
}

func TestWindowsStartupFailureStateJoinsInputAfterReadinessWasConsumed(t *testing.T) {
	wantInput := errors.New("input writer retired")
	status := make(chan windowsStatusResult, 1)
	status <- windowsStatusResult{err: errors.New("settlement unavailable")}
	input := make(chan error, 1)
	input <- wantInput
	stderr := make(chan error, 1)
	stderr <- nil
	wait := make(chan error, 1)
	wait <- nil
	state := windowsStartupJoinState{readinessReady: true}
	if !state.collect(make(chan error), status, input, stderr, wait, time.Second) {
		t.Fatalf("startup tasks did not join: %v", state.pending())
	}
	if !errors.Is(state.inputErr, wantInput) {
		t.Fatalf("input join error = %v", state.inputErr)
	}
}

func TestWindowsOwnedOutputDrainContinuesAfterCaptureLimit(t *testing.T) {
	source := bytes.NewReader(bytes.Repeat([]byte("output"), MaximumCapturedOutputBytes+64<<10))
	capture := newBoundedOutput("stdout")
	err := drainOwnedOutput(source, capture, "owned stdout")
	if !errors.Is(err, ErrOutputCaptureLimit) || !strings.Contains(err.Error(), "owned stdout capture") {
		t.Fatalf("drain error = %v", err)
	}
	if source.Len() != 0 {
		t.Fatalf("capture limit left %d process-owned bytes undrained", source.Len())
	}
	snapshot := capture.snapshot()
	if !snapshot.Truncated || len(snapshot.Bytes) != MaximumCapturedOutputBytes {
		t.Fatalf("bounded capture = bytes=%d truncated=%t", len(snapshot.Bytes), snapshot.Truncated)
	}
}

func TestWindowsSessionInputRetirementIsIdempotentAfterWriterCompletion(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	session := &windowsSession{inputWriter: writer}
	if err := session.closeInput(); err != nil {
		t.Fatalf("adopt completed input close: %v", err)
	}
	if err := session.closeInput(); err != nil {
		t.Fatalf("repeat completed input close: %v", err)
	}
}
