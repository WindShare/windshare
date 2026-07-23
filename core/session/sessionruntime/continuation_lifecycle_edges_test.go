package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
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
		call := &operationCall{}
		ctx := newSecondObservationCanceledContext()
		if _, err := call.acquireCandidateSend(ctx, context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-acquisition caller cancellation error=%v", err)
		}
		assertCandidateGateReusable(t, call)
	})

	t.Run("runtime cancellation", func(t *testing.T) {
		call := &operationCall{}
		lifetime := newSecondObservationCanceledContext()
		if _, err := call.acquireCandidateSend(context.Background(), lifetime); !errors.Is(err, ErrRuntimeClosed) {
			t.Fatalf("post-acquisition runtime cancellation error=%v", err)
		}
		assertCandidateGateReusable(t, call)
	})
}

func assertCandidateGateReusable(t *testing.T, call *operationCall) {
	t.Helper()
	release, err := call.acquireCandidateSend(context.Background(), context.Background())
	if err != nil {
		t.Fatalf("candidate gate token leaked: %v", err)
	}
	release()
}

func TestOperationCallClosedStateRejectsEveryAuthorityMutation(t *testing.T) {
	if _, err := (*operationCall)(nil).acquireCandidateSend(context.Background(), context.Background()); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("nil candidate call error=%v", err)
	}
	if _, err := (&operationCall{}).acquireCandidateSend(nil, context.Background()); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("nil candidate context error=%v", err)
	}

	call := &operationCall{closed: true}
	if _, err := call.acquireCandidateSend(context.Background(), context.Background()); !errors.Is(err, ErrOperationMissing) {
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
	if err := call.queueRequestReplay(nil); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("closed replay queue error=%v", err)
	}

	open := &operationCall{}
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

func TestReceiverPeerOperationReportsGenerationContinuationBudget(t *testing.T) {
	if maximum, ok := (*ReceiverPeerOperation)(nil).MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("nil peer continuation budget=%d,%t", maximum, ok)
	}
	if maximum, ok := (&ReceiverPeerOperation{}).MaximumContinuations(); ok || maximum != 0 {
		t.Fatalf("unconstructed peer continuation budget=%d,%t", maximum, ok)
	}

	t.Run("local terminal retains admitted budget", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 140)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		termination := fixture.operation.Terminate(context.Background())
		assertReceiverPeerTermination(
			t,
			fixture.operation,
			termination,
			ReceiverPeerTerminalAuthorityLocal,
			ReceiverPeerProvenanceLocalExplicitStop,
			ReceiverPeerTerminalOperationOnly,
			ReceiverPeerProvenanceLocalExplicitStop,
		)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
	})

	t.Run("runtime shutdown retains admitted budget", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 141)
		if err := fixture.runtime.addFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}
		fixture.runtime.abortBeforeStart()
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
		termination := fixture.operation.Terminate(context.Background())
		assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)
		assertReceiverPeerContinuationBudget(t, fixture.operation, 4)
	})

	t.Run("call close retains admission snapshot", func(t *testing.T) {
		fixture := newTrackedContinuationReplayFixture(t, 142)
		if maximum, ok := fixture.call.continuationLimit(); !ok || maximum != 4 {
			t.Fatalf("live call continuation snapshot=%d,%t", maximum, ok)
		}
		fixture.call.close()
		if maximum, ok := fixture.call.continuationLimit(); !ok || maximum != 4 {
			t.Fatalf("closed call continuation snapshot=%d,%t", maximum, ok)
		}
	})
}

func assertReceiverPeerContinuationBudget(
	t *testing.T,
	operation *ReceiverPeerOperation,
	want int,
) {
	t.Helper()
	if maximum, ok := operation.MaximumContinuations(); !ok || maximum != want {
		t.Fatalf("peer continuation budget=%d,%t want=%d,true", maximum, ok, want)
	}
}

func TestSenderPeerFunctionAdapterRejectsNilContinuationClassifier(t *testing.T) {
	var nilFactory SenderPeerHandlerFactoryFunc
	if _, _, err := nilFactory.BeginOperationContinuation(
		protocolsession.MessagePeerOffer,
		[]byte{0xf6},
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil begin classifier error=%v", err)
	}
	if _, _, err := nilFactory.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate,
		[]byte{0xf6},
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil unbound classifier error=%v", err)
	}

	factory := SenderPeerHandlerFactoryFunc(func(SenderPeerSession) (SenderPeerHandler, error) {
		return inertSenderPeerHandler{}, nil
	})
	if _, recognized, err := factory.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate,
		[]byte{0xf6},
	); err != nil || recognized {
		t.Fatalf("function adapter unbound classification recognized=%t error=%v", recognized, err)
	}
}
