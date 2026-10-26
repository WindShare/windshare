package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync/atomic"
	"testing"
)

type secondObservationCanceledContext struct {
	context.Context
	done  chan struct{}
	calls atomic.Int32
}

func newSecondObservationCanceledContext() *secondObservationCanceledContext {
	done := make(chan struct{})
	close(done)
	return &secondObservationCanceledContext{Context: context.Background(), done: done}
}

func (ctx *secondObservationCanceledContext) Done() <-chan struct{} {
	if ctx.calls.Add(1) == 1 {
		return nil
	}
	return ctx.done
}

func (ctx *secondObservationCanceledContext) Err() error {
	if ctx.calls.Load() > 1 {
		return context.Canceled
	}
	return nil
}

func TestOperationCandidateGateRechecksCancellationAfterOwnershipTransfer(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		call := &Call{}
		ctx := newSecondObservationCanceledContext()
		if _, err := call.acquireCandidateSend(ctx, context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-acquisition caller cancellation error=%v", err)
		}
		assertCandidateGateReusable(t, call)
	})

	t.Run("runtime cancellation", func(t *testing.T) {
		call := &Call{}
		lifetime := newSecondObservationCanceledContext()
		if _, err := call.acquireCandidateSend(context.Background(), lifetime); !errors.Is(err, session.ErrRuntimeClosed) {
			t.Fatalf("post-acquisition runtime cancellation error=%v", err)
		}
		assertCandidateGateReusable(t, call)
	})
}

func assertCandidateGateReusable(t *testing.T, call *Call) {
	t.Helper()
	release, err := call.acquireCandidateSend(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("candidate gate token leaked: %v", err)
	}
	release()
}

func TestOperationCallClosedStateRejectsEveryAuthorityMutation(t *testing.T) {
	if _, err := (*Call)(nil).acquireCandidateSend(context.Background(), context.Background()); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("nil candidate call error=%v", err)
	}
	// A named nil value keeps this deliberate invalid-input contract distinct from
	// production call sites, which must always propagate an owned context.
	var missingContext context.Context
	if _, err := (&Call{}).acquireCandidateSend(missingContext, context.Background()); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("nil candidate context error=%v", err)
	}

	call := &Call{closed: true}
	if _, err := call.acquireCandidateSend(context.Background(), context.Background()); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("closed candidate call error=%v", err)
	}
	done := call.doneChannel()
	select {
	case <-done:
	default:
		t.Fatal("closed call created a live completion channel")
	}
	if err := call.enqueue(operationResponse{}); err != nil {
		t.Fatalf("late response to a closed sink error=%v", err)
	}
	if call.setAuthority(protocolsession.OperationGeneration{}, protocolsession.OutboundOperationPermit{}) {
		t.Fatal("closed call accepted generation authority")
	}
	if call.setRequestReplay(protocolsession.Message{}, protocolsession.OutboundReplayPermit{}) {
		t.Fatal("closed call accepted replay authority")
	}
	if err := call.queueRequestReplay(nil); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("closed replay queue error=%v", err)
	}

	open := &Call{}
	if open.setRequestReplay(protocolsession.Message{}, protocolsession.OutboundReplayPermit{}) {
		t.Fatal("zero replay capability was accepted")
	}
}

func TestOperationRequestReplayRetainsAuthorityWhenReplacementWriterRejects(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 138)
	if err := fixture.call.queueRequestReplay(nil); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("nil replacement writer error=%v", err)
	}
	stopWriterBeforeAdmission(t, fixture.replacement.writer)
	if err := fixture.call.queueRequestReplay(fixture.replacement.writer); !errors.Is(err, protocolsession.ErrWriterStopped) {
		t.Fatalf("stopped replacement writer error=%v", err)
	}
	fixture.call.stateMu.Lock()
	replayRetained := !fixture.call.replay.IsZero()
	fixture.call.stateMu.Unlock()
	if !replayRetained {
		t.Fatal("failed replacement consumed exact request replay authority")
	}
}

func TestContinuationAdmissionRetiresOnlyTheStoppedAttemptedLane(t *testing.T) {
	fixture := newContinuationReplayFixture(t, 139)
	if !fixture.rpc.retryContinuationAdmission(
		context.Background(),
		0,
		fixture.preferred,
		protocolsession.ErrWriterStopped,
	) {
		t.Fatal("stopped preferred writer was not classified for one replacement retry")
	}
	fixture.runtime.lanes.mu.Lock()
	preferredClosing := fixture.runtime.lanes.active[fixture.preferred.identity.ID].closing
	fixture.runtime.lanes.mu.Unlock()
	if !preferredClosing {
		t.Fatal("failed continuation lane remained selectable")
	}
	selected, err := fixture.rpc.continuationLane(fixture.call)
	if err != nil || selected.identity != fixture.replacement.identity {
		t.Fatalf("replacement continuation lane=%+v error=%v", selected.identity, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if fixture.rpc.retryContinuationAdmission(
		canceled,
		0,
		selected,
		protocolsession.ErrWriterStopped,
	) {
		t.Fatal("caller cancellation authorized another lane retry")
	}
	fixture.runtime.lanes.mu.Lock()
	replacementClosing := fixture.runtime.lanes.active[selected.identity.ID].closing
	fixture.runtime.lanes.mu.Unlock()
	if replacementClosing {
		t.Fatal("canceled continuation retired an unattempted replacement lane")
	}
}
