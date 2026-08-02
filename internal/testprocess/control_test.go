package testprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func TestControlPublicationRetryBoundaryFollowsWrittenBytes(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	control := protocol.Control{
		SchemaVersion: protocol.ControlSchemaVersion,
		Identity:      identity,
		Reason:        protocol.ControlReasonStop,
	}
	preWriteFailure := errors.New("endpoint unavailable")
	err := publishControl(fixedFailureWriter{err: preWriteFailure}, control)
	if !errors.Is(err, preWriteFailure) || !retryableControlPublication(err) {
		t.Fatalf("pre-write publication error = %v, retryable=%t", err, retryableControlPublication(err))
	}
	partialFailure := errors.New("stream failed after bytes")
	err = publishControl(fixedFailureWriter{written: 2, err: partialFailure}, control)
	if !errors.Is(err, partialFailure) || retryableControlPublication(err) {
		t.Fatalf("partial publication error = %v, retryable=%t", err, retryableControlPublication(err))
	}
	var encoded bytes.Buffer
	if err := publishControl(&encoded, control); err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.ReadFrame[protocol.Control](&encoded)
	if err != nil || decoded != control {
		t.Fatalf("published control = %#v, %v", decoded, err)
	}
}

func TestProcessRetriesOnlyZeroByteControlFailure(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	settlement := successfulSettlement(identity, 0)
	settlement.TerminationReason = protocol.TerminationStop
	preWriteErr := &controlPublicationError{cause: errors.New("not connected")}
	session := newScriptedStopSession(settlement, []error{preWriteErr, nil})
	process := newProcess(identity, session, func() {})
	if err := process.requestStop(protocol.ControlReasonStop); !errors.Is(err, preWriteErr) {
		t.Fatalf("first stop error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := process.Stop(ctx)
	if err != nil || result.TerminationReason != protocol.TerminationStop || session.stopCount() != 2 {
		t.Fatalf("retried Stop = %#v, %v; attempts=%d", result, err, session.stopCount())
	}
}

func TestProcessNeverRetriesPoisonedControlStream(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	settlement := successfulSettlement(identity, 0)
	settlement.TerminationReason = protocol.TerminationStop
	partialErr := &controlPublicationError{cause: errors.New("partial control"), bytesWritten: 1}
	session := newScriptedStopSession(settlement, []error{partialErr})
	process := newProcess(identity, session, func() {})
	if err := process.requestStop(protocol.ControlReasonStop); !errors.Is(err, partialErr) {
		t.Fatalf("first stop error = %v", err)
	}
	session.settle()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, firstErr := process.Wait(ctx)
	_, secondErr := process.Stop(ctx)
	if !errors.Is(firstErr, partialErr) || firstErr != secondErr {
		t.Fatalf("cached terminal errors = first %v, second %v", firstErr, secondErr)
	}
	if session.stopCount() != 1 {
		t.Fatalf("poisoned control stream attempts = %d", session.stopCount())
	}
}

type fixedFailureWriter struct {
	written int
	err     error
}

func (writer fixedFailureWriter) Write(value []byte) (int, error) {
	written := writer.written
	if written > len(value) {
		written = len(value)
	}
	return written, writer.err
}

type scriptedStopSession struct {
	settlement protocol.Settlement
	stopErrors []error
	release    chan struct{}
	stopOnce   sync.Once

	mu       sync.Mutex
	attempts int
}

func newScriptedStopSession(settlement protocol.Settlement, stopErrors []error) *scriptedStopSession {
	return &scriptedStopSession{
		settlement: settlement,
		stopErrors: append([]error(nil), stopErrors...),
		release:    make(chan struct{}),
	}
}

func (session *scriptedStopSession) wait() (protocol.Settlement, error) {
	<-session.release
	return session.settlement, nil
}

func (session *scriptedStopSession) stop(protocol.Control) error {
	session.mu.Lock()
	index := session.attempts
	session.attempts++
	var err error
	if index < len(session.stopErrors) {
		err = session.stopErrors[index]
	}
	session.mu.Unlock()
	if err == nil {
		session.settle()
	}
	return err
}

func (session *scriptedStopSession) settle() {
	session.stopOnce.Do(func() { close(session.release) })
}

func (session *scriptedStopSession) close() error          { return nil }
func (session *scriptedStopSession) events() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }

func (session *scriptedStopSession) stopCount() int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.attempts
}
