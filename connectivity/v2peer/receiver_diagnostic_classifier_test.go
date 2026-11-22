package v2peer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type uncomparableReceiverError []byte

func (uncomparableReceiverError) Error() string { return "uncomparable receiver failure" }

func TestReceiverResidualClassifierTreatsBareContextCanceledAsBenign(t *testing.T) {
	classified := classifyReceiverCause(context.Canceled, receiverCausePolicy{contextCanceled: true})
	if classified.retained != nil ||
		!containsReceiverBenignCause(classified.benign, ReceiverBenignContextCanceled) {
		t.Fatalf("bare context cancellation classification=%+v", classified)
	}
}

func TestReceiverResidualClassifierRetainsUncomparableError(t *testing.T) {
	genuineFailure := uncomparableReceiverError{1, 2, 3}
	classified := classifyReceiverCause(
		errors.Join(context.Canceled, genuineFailure),
		receiverCausePolicy{contextCanceled: true},
	)
	if classified.retained == nil || classified.retained.Error() != genuineFailure.Error() ||
		len(classified.benign) != 1 || classified.benign[0] != ReceiverBenignContextCanceled {
		t.Fatalf("uncomparable error classification=%+v", classified)
	}
}

func TestReceiverResidualClassifierDoesNotReintroduceFilteredWrappedLeaf(t *testing.T) {
	genuineFailure := errors.New("wrapped cleanup conflict")
	cause := fmt.Errorf(
		"terminate exact peer operation: %w",
		errors.Join(session.ErrOperationMissing, genuineFailure),
	)
	classified := classifyReceiverCause(cause, receiverCausePolicy{
		operationMissing: ReceiverBenignRemoteOperationMissing,
	})
	if !errors.Is(classified.retained, genuineFailure) ||
		errors.Is(classified.retained, session.ErrOperationMissing) ||
		!strings.HasPrefix(classified.retained.Error(), "terminate exact peer operation:") ||
		!containsReceiverBenignCause(classified.benign, ReceiverBenignRemoteOperationMissing) {
		t.Fatalf("wrapped mixed classification=%+v", classified)
	}
}

func TestReceiverResidualClassifierPreservesLeafAndWrappedIdentity(t *testing.T) {
	genuine := errors.New("receiver diagnostic identity")
	classified := classifyReceiverCause(genuine, receiverCausePolicy{})
	if !errors.Is(classified.retained, genuine) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("leaf classification=%+v", classified)
	}

	wrapped := fmt.Errorf("create local offer: %w", genuine)
	classified = classifyReceiverCause(wrapped, receiverCausePolicy{})
	if !errors.Is(classified.retained, genuine) || classified.retained.Error() != wrapped.Error() {
		t.Fatalf("wrapped classification=%+v residual=%v", classified, classified.retained)
	}

	multi := fmt.Errorf("terminate peer operation: %w; cleanup: %w", context.Canceled, genuine)
	classified = classifyReceiverCause(multi, receiverCausePolicy{contextCanceled: true})
	if !errors.Is(classified.retained, genuine) ||
		errors.Is(classified.retained, context.Canceled) ||
		!containsReceiverBenignCause(classified.benign, ReceiverBenignContextCanceled) {
		t.Fatalf("multi-wrapper classification=%+v residual=%v", classified, classified.retained)
	}
}

func TestReceiverResidualClassifierHandlesBoundaryAndRemoteErrors(t *testing.T) {
	sessionFault, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionProtocol,
	)
	coreFailure := transferfault.Wrap(sessionFault, protocolsession.ErrInvalidOperationFailure)
	classified := classifyReceiverCause(coreFailure, receiverCausePolicy{})
	if !errors.Is(classified.retained, protocolsession.ErrInvalidOperationFailure) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
		containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("boundary classification=%+v", classified)
	}
	if _, ok := errors.AsType[*transferfault.BoundaryError](classified.retained); ok {
		t.Fatal("core boundary fault escaped the receiver residual")
	}

	var typedNil *transferfault.BoundaryError
	nilBoundary := classifyReceiverCause(typedNil, receiverCausePolicy{})
	if !errors.Is(nilBoundary.retained, errReceiverOpaqueCause) ||
		!containsReceiverCauseClass(nilBoundary.classes, ReceiverCauseUnknown) {
		t.Fatalf("typed-nil boundary classification=%+v", nilBoundary)
	}

	want := receiversession.RemoteOperationFailureSnapshot{}
	remote := receiversession.NewRemoteOperationError(want)
	classified = classifyReceiverCause(errors.Join(remote, ErrProtocol), receiverCausePolicy{})
	var got receiversession.RemoteOperationError
	if !errors.As(classified.retained, &got) || got.Failure() != want ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) {
		t.Fatalf("remote diagnostic classification=%+v", classified)
	}
}

func TestReceiverResidualClassifierBoundsExcessiveDepth(t *testing.T) {
	deep := error(errors.New("deep receiver leaf"))
	for depth := range maxReceiverErrorDepth + 8 {
		deep = fmt.Errorf("depth %d: %w", depth, deep)
	}
	classified := classifyReceiverCause(deep, receiverCausePolicy{})
	if classified.complete || !errors.Is(classified.retained, errReceiverOpaqueCause) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("depth-bounded classification=%+v", classified)
	}
}

func TestReceiverDecisionMergeSeparatesTransitionFromUnsafeConsequence(t *testing.T) {
	local := receiverOperationDecision(
		ReceiverTerminalLocal,
		ReceiverProvenanceLocalExplicitStop,
	)
	unsafe := receiverUnsafeConsequence(
		ReceiverProvenanceAuthenticatedCandidateBindingMismatch,
	)
	for name, merged := range map[string]receiverAttemptDecision{
		"unsafe first": mergeReceiverAttemptDecisions(unsafe, local),
		"local first":  mergeReceiverAttemptDecisions(local, unsafe),
	} {
		t.Run(name, func(t *testing.T) {
			if merged.transitionOwner != ReceiverTerminalLocal ||
				merged.transitionProvenance != ReceiverProvenanceLocalExplicitStop ||
				merged.disposition != ReceiverDispositionSessionUnsafe ||
				merged.consequenceProvenance != ReceiverProvenanceAuthenticatedCandidateBindingMismatch {
				t.Fatalf("merged receiver decision=%+v", merged)
			}
		})
	}
}

func TestReceiverTerminationTraceContainsSafeCorrelationAndClassification(t *testing.T) {
	var operation *exactReceiverTestOperation
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		config.ReceiverTerminationObservationCapacity = 1
		operation = newExactReceiverTestOperation(
			signaling.operation.(*receiverTestOperation),
			false,
		)
		signaling.operation = operation
	})
	traces := harness.factory.ReceiverTerminationObservations()
	genuineConflict := errors.New("secret diagnostic text must not enter trace")
	operation.terminateCause = errors.Join(context.Canceled, genuineConflict)

	if err := harness.attempt.Close(); !errors.Is(err, genuineConflict) {
		t.Fatalf("trace harness Close=%v", err)
	}
	trace := receiveTest(t, traces)
	if trace.OperationID().IsZero() || trace.LocalGeneration() == 0 ||
		trace.TransitionAuthority() != ReceiverTerminalLocal ||
		trace.Disposition() != ReceiverDispositionFallbackAllowed ||
		trace.TransitionProvenance() != ReceiverProvenanceLocalExplicitStop ||
		trace.ConsequenceProvenance() != ReceiverProvenanceLocalExplicitStop ||
		len(trace.BenignComponents()) == 0 || len(trace.RetainedCauseClasses()) == 0 {
		t.Fatalf("termination trace=%+v", trace)
	}
}

func TestReceiverAttemptLateSameIDCloseCannotCancelReplacementObject(t *testing.T) {
	first, firstOperation := newReceiverShutdownHarness(t, true)
	firstOperation.remoteError <- session.ErrOperationMissing
	receiveTest(t, firstOperation.remoteTerminal)

	second, secondOperation := newReceiverShutdownHarness(t, false)
	if firstOperation.receiverTestOperation.id != secondOperation.receiverTestOperation.id {
		t.Fatal("test did not force same OperationID reuse")
	}
	firstClosed := make(chan error, 1)
	go func() { firstClosed <- first.attempt.Close() }()
	firstOperation.releaseRemoteError()
	if err := receiveTest(t, firstClosed); err != nil {
		t.Fatalf("late first-generation Close: %v", err)
	}
	if secondOperation.terminateCalls.Load() != 0 || secondOperation.OperationID().IsZero() {
		t.Fatal("late first-generation Close mutated replacement signaling object")
	}

	second.answer(t)
	second.openAndAwaitLane(t)
	if err := second.attempt.Close(); err != nil {
		t.Fatalf("replacement same-ID receiver path failed: %v", err)
	}
	if calls := secondOperation.terminateCalls.Load(); calls != 1 {
		t.Fatalf("replacement exact operation Terminate calls=%d, want 1", calls)
	}
	firstOutcome, secondOutcome := first.attempt.Outcome(), second.attempt.Outcome()
	if firstOutcome.OperationID() != secondOutcome.OperationID() ||
		firstOutcome.LocalGeneration() == 0 || secondOutcome.LocalGeneration() == 0 ||
		firstOutcome.LocalGeneration() == secondOutcome.LocalGeneration() {
		t.Fatalf("same-ID local generations first=%+v second=%+v", firstOutcome, secondOutcome)
	}
}

func containsReceiverBenignCause(
	causes []ReceiverBenignCause,
	want ReceiverBenignCause,
) bool {
	return slices.Contains(causes, want)
}

func containsReceiverCauseClass(classes []ReceiverCauseClass, want ReceiverCauseClass) bool {
	return slices.Contains(classes, want)
}
