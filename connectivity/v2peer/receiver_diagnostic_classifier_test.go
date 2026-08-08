package v2peer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
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

func TestReceiverResidualClassifierFailSafeRetainsUncomparableError(t *testing.T) {
	genuineFailure := uncomparableReceiverError{1, 2, 3}
	classified := classifyReceiverCause(
		errors.Join(context.Canceled, genuineFailure),
		receiverCausePolicy{contextCanceled: true},
	)
	if classified.retained == nil || len(classified.benign) != 1 ||
		classified.benign[0] != ReceiverBenignContextCanceled {
		t.Fatalf("uncomparable error classification=%+v", classified)
	}
}

func TestReceiverResidualClassifierDoesNotReintroduceFilteredWrappedLeaf(t *testing.T) {
	genuineFailure := errors.New("wrapped cleanup conflict")
	cause := fmt.Errorf(
		"terminate exact peer operation: %w",
		errors.Join(sessionruntime.ErrOperationMissing, genuineFailure),
	)
	classified := classifyReceiverCause(cause, receiverCausePolicy{
		operationMissing: ReceiverBenignRemoteOperationMissing,
	})
	if !errors.Is(classified.retained, genuineFailure) ||
		errors.Is(classified.retained, sessionruntime.ErrOperationMissing) ||
		!strings.HasPrefix(classified.retained.Error(), "terminate exact peer operation:") ||
		!containsReceiverBenignCause(classified.benign, ReceiverBenignRemoteOperationMissing) {
		t.Fatalf("wrapped mixed classification=%+v", classified)
	}
}

type receiverJoinedTestError struct {
	children []error
}

type receiverSingleCycleError struct{}

func (*receiverSingleCycleError) Error() string { return "single unwrap cycle" }
func (failure *receiverSingleCycleError) Unwrap() error {
	return failure
}

type receiverMultiCycleError struct{}

func (*receiverMultiCycleError) Error() string { return "multi unwrap cycle" }
func (failure *receiverMultiCycleError) Unwrap() []error {
	return []error{failure}
}

type receiverBinaryCycleError struct {
	unwrapCalls int
}

func (*receiverBinaryCycleError) Error() string { return "binary unwrap cycle" }
func (failure *receiverBinaryCycleError) Unwrap() []error {
	failure.unwrapCalls++
	return []error{failure, failure}
}

type receiverUncomparableCycleError []byte

func (receiverUncomparableCycleError) Error() string { return "uncomparable binary unwrap cycle" }
func (failure receiverUncomparableCycleError) Unwrap() []error {
	return []error{failure, failure}
}

type receiverDeepWrapperError struct {
	next error
}

type receiverStatefulWrapperError struct {
	unwrapCalls int
}

type receiverTypedNilWrapperError struct{}

type receiverComparableTrapError struct{ value any }

type receiverBoundaryWrapper struct {
	child       error
	unwrapCalls int
}

func (*receiverDeepWrapperError) Error() string { return "deep receiver wrapper" }
func (failure *receiverDeepWrapperError) Unwrap() error {
	return failure.next
}

func (*receiverStatefulWrapperError) Error() string { return "stateful receiver wrapper" }
func (failure *receiverStatefulWrapperError) Unwrap() error {
	failure.unwrapCalls++
	if failure.unwrapCalls == 1 {
		return ErrProtocol
	}
	return ErrNegotiation
}

func (*receiverTypedNilWrapperError) Error() string { return "typed-nil receiver wrapper" }
func (*receiverTypedNilWrapperError) Unwrap() error {
	panic("typed-nil receiver wrapper must remain opaque")
}

func (receiverComparableTrapError) Error() string { return "comparable receiver trap" }

func (*receiverBoundaryWrapper) Error() string { return "wrapped core boundary fault" }
func (failure *receiverBoundaryWrapper) Unwrap() error {
	failure.unwrapCalls++
	return failure.child
}

func (*receiverJoinedTestError) Error() string { return "custom joined receiver failure" }
func (failure *receiverJoinedTestError) Unwrap() []error {
	return failure.children
}

func TestReceiverResidualClassifierFailsClosedForNilJoinedChildren(t *testing.T) {
	for _, test := range []struct {
		name     string
		children []error
	}{
		{name: "empty"},
		{name: "all nil", children: []error{nil, nil}},
		{name: "mixed nil", children: []error{context.Canceled, nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &receiverJoinedTestError{children: test.children}
			classified := classifyReceiverCause(cause, receiverCausePolicy{contextCanceled: true})
			if classified.retained != errReceiverOpaqueCause || len(classified.benign) != 0 ||
				!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
				t.Fatalf("nil-child joined classification=%+v", classified)
			}
		})
	}
}

func TestReceiverCauseClassTraversalTreatsUnknownWrapperAsOpaque(t *testing.T) {
	cause := &receiverStatefulWrapperError{}
	classes := ReceiverCauseClasses(cause)
	if cause.unwrapCalls != 0 {
		t.Fatalf("stateful wrapper unwrapped %d times", cause.unwrapCalls)
	}
	if len(classes) != 1 || classes[0] != ReceiverCauseUnknown {
		t.Fatalf("stateful wrapper classes=%v", classes)
	}
}

func TestReceiverUnknownWrapperCannotEraseItsOwnFailureIdentity(t *testing.T) {
	cause := &receiverDeepWrapperError{next: context.Canceled}
	classified := classifyReceiverCause(cause, receiverCausePolicy{contextCanceled: true})
	if classified.retained != errReceiverOpaqueCause || len(classified.benign) != 0 ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("unknown wrapper classification=%+v", classified)
	}
}

func TestReceiverTypedNilWrapperBecomesStableOpaqueCause(t *testing.T) {
	var typedNil *receiverTypedNilWrapperError
	classified := classifyReceiverCause(typedNil, receiverCausePolicy{})
	if classified.retained != errReceiverOpaqueCause || len(classified.benign) != 0 ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("typed-nil classification=%+v", classified)
	}
}

func TestReceiverUnauditedComparableLeafCannotEscapeSafeEquality(t *testing.T) {
	trap := receiverComparableTrapError{value: []byte{1}}
	classified := classifyReceiverCause(trap, receiverCausePolicy{})
	if classified.retained != errReceiverOpaqueCause ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("comparable trap classification=%+v", classified)
	}
	// A raw trap panics when errors.Is compares two values of its statically
	// comparable type because the interface field dynamically contains a slice.
	if errors.Is(classified.retained, receiverComparableTrapError{value: []byte{1}}) {
		t.Fatal("opaque receiver failure matched unaudited comparable leaf")
	}
}

func TestReceiverTrustedErrorsNewLeafPreservesIdentity(t *testing.T) {
	genuine := errors.New("receiver diagnostic identity")
	classified := classifyReceiverCause(genuine, receiverCausePolicy{})
	if !errors.Is(classified.retained, genuine) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("trusted errors.New classification=%+v", classified)
	}
}

func TestReceiverTrustedFmtWrapperPreservesDiagnosticOverSafeResidual(t *testing.T) {
	genuine := errors.New("receiver wrapped identity")
	cause := fmt.Errorf("create local offer: %w", genuine)
	classified := classifyReceiverCause(cause, receiverCausePolicy{})
	if !errors.Is(classified.retained, genuine) || classified.retained.Error() != cause.Error() ||
		reflect.TypeOf(classified.retained) != receiverSafeDiagnosticType {
		t.Fatalf("trusted fmt classification=%+v residual=%v", classified, classified.retained)
	}
}

func TestReceiverTrustedFmtMultiWrapperOwnsFilteredResidual(t *testing.T) {
	genuine := errors.New("receiver multi-wrapper diagnostic identity")
	cause := fmt.Errorf("terminate peer operation: %w; cleanup: %w", context.Canceled, genuine)
	classified := classifyReceiverCause(cause, receiverCausePolicy{contextCanceled: true})
	if !errors.Is(classified.retained, genuine) ||
		errors.Is(classified.retained, context.Canceled) ||
		classified.retained.Error() != cause.Error() ||
		reflect.TypeOf(classified.retained) != receiverSafeDiagnosticType ||
		!containsReceiverBenignCause(classified.benign, ReceiverBenignContextCanceled) {
		t.Fatalf("trusted fmt multi-wrapper classification=%+v residual=%v", classified, classified.retained)
	}
}

func TestReceiverClosedSessionFaultBecomesOwnedProtocolDiagnostic(t *testing.T) {
	unsafeChild := &receiverStatefulWrapperError{}
	coreFailure := receiverSessionBoundaryError(unsafeChild)
	classified := classifyReceiverCause(coreFailure, receiverCausePolicy{})
	if unsafeChild.unwrapCalls != 0 ||
		!errors.Is(classified.retained, errReceiverOpaqueCause) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("core session failure classification=%+v unwrap_calls=%d", classified, unsafeChild.unwrapCalls)
	}
	if _, ok := errors.AsType[*transferfault.BoundaryError](classified.retained); ok {
		t.Fatal("core boundary fault escaped the owned receiver residual")
	}
}

func receiverSessionBoundaryError(cause error) error {
	value, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionProtocol,
	)
	return transferfault.Wrap(value, cause)
}

func TestReceiverCrossScopeSessionFailureRequiresSealedAuthority(t *testing.T) {
	classified := classifyReceiverCause(
		receiverSessionBoundaryError(protocolsession.ErrInvalidOperationFailure),
		receiverCausePolicy{},
	)
	if !errors.Is(classified.retained, protocolsession.ErrInvalidOperationFailure) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
		containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("cross-scope session failure classification=%+v", classified)
	}
	if _, ok := errors.AsType[*transferfault.BoundaryError](classified.retained); ok {
		t.Fatal("cross-scope core boundary escaped the owned receiver residual")
	}
	decision := receiverUnsafeConsequence(ReceiverProvenanceRemoteFailureScopeViolation)
	classified = annotateReceiverDecisionDiagnostics(
		classified,
		decision,
	)
	if decision.disposition != ReceiverDispositionSessionUnsafe ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) {
		t.Fatalf("sealed session consequence=%+v decision=%+v", classified, decision)
	}

	bare := classifyReceiverCause(protocolsession.ErrInvalidOperationFailure, receiverCausePolicy{})
	if !containsReceiverCauseClass(bare.classes, ReceiverCauseProtocol) {
		t.Fatalf("bare invalid-operation classification=%+v", bare)
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

func TestReceiverUntrustedBoundaryWrapperRemainsOpaque(t *testing.T) {
	wrapped := &receiverBoundaryWrapper{
		child: receiverSessionBoundaryError(protocolsession.ErrInvalidOperationFailure),
	}
	classified := classifyReceiverCause(wrapped, receiverCausePolicy{})
	if classified.retained != errReceiverOpaqueCause ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("untrusted boundary classification=%+v", classified)
	}
	if wrapped.unwrapCalls != 0 {
		t.Fatalf("unknown session-failure wrapper unwrapped %d times", wrapped.unwrapCalls)
	}

	var typedNil *transferfault.BoundaryError
	typedNilClassified := classifyReceiverCause(typedNil, receiverCausePolicy{})
	if typedNilClassified.retained != errReceiverOpaqueCause ||
		!containsReceiverCauseClass(typedNilClassified.classes, ReceiverCauseUnknown) {
		t.Fatalf("typed-nil core session failure classification=%+v", typedNilClassified)
	}
}

func TestReceiverDepthTruncationCannotConsumeLaterSiblingBudget(t *testing.T) {
	deep := error(errors.New("unreachable receiver leaf"))
	for depth := range maximumReceiverErrorTreeDepth + 8 {
		deep = fmt.Errorf("trusted receiver depth %d: %w", depth, deep)
	}
	diagnostic := receiverSessionBoundaryError(protocolsession.ErrInvalidOperationFailure)
	for _, cause := range []error{
		errors.Join(diagnostic, deep),
		errors.Join(deep, diagnostic),
	} {
		classified := classifyReceiverCause(cause, receiverCausePolicy{})
		if classified.complete ||
			!errors.Is(classified.retained, protocolsession.ErrInvalidOperationFailure) ||
			!errors.Is(classified.retained, errReceiverOpaqueCause) ||
			!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
			!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
			t.Fatalf("order-independent bounded diagnostic=%+v", classified)
		}
	}
}

func TestReceiverWideDiagnosticOrderCannotChangeSealedSessionAuthority(t *testing.T) {
	diagnostic := receiverSessionBoundaryError(protocolsession.ErrInvalidOperationFailure)
	decision := receiverUnsafeConsequence(ReceiverProvenanceRemoteFailureScopeViolation)
	for _, fatalIndex := range []int{0, maximumReceiverErrorTreeNodes} {
		children := make([]error, maximumReceiverErrorTreeNodes+1)
		for index := range children {
			children[index] = context.DeadlineExceeded
		}
		children[fatalIndex] = diagnostic
		classified := classifyReceiverCause(errors.Join(children...), receiverCausePolicy{})
		if classified.complete ||
			!containsReceiverCauseClass(classified.classes, ReceiverCauseDeadline) ||
			!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
			t.Fatalf("wide diagnostic classification=%+v", classified)
		}
		annotated := annotateReceiverDecisionDiagnostics(
			classified,
			decision,
		)
		if decision.disposition != ReceiverDispositionSessionUnsafe ||
			!containsReceiverCauseClass(annotated.classes, ReceiverCauseProtocol) {
			t.Fatalf("wide sealed session classification=%+v decision=%+v", annotated, decision)
		}
	}
}

func TestReceiverAttemptSealedSessionConsequenceSurvivesDiagnosticOrder(t *testing.T) {
	deep := error(errors.New("unreachable structural-scope diagnostic"))
	for depth := range maximumReceiverErrorTreeDepth + 8 {
		deep = fmt.Errorf("trusted structural-scope depth %d: %w", depth, deep)
	}
	sessionDiagnostic := receiverSessionBoundaryError(protocolsession.ErrInvalidOperationFailure)
	wideFirst := make([]error, maximumReceiverErrorTreeNodes+1)
	wideLast := make([]error, maximumReceiverErrorTreeNodes+1)
	for index := range wideFirst {
		wideFirst[index] = context.DeadlineExceeded
		wideLast[index] = context.DeadlineExceeded
	}
	wideFirst[0] = sessionDiagnostic
	wideLast[len(wideLast)-1] = sessionDiagnostic

	for _, test := range []struct {
		name         string
		cause        error
		wantDeadline bool
	}{
		{name: "depth session first", cause: errors.Join(sessionDiagnostic, deep)},
		{name: "depth session last", cause: errors.Join(deep, sessionDiagnostic)},
		{name: "wide session first", cause: errors.Join(wideFirst...), wantDeadline: true},
		{name: "wide session last", cause: errors.Join(wideLast...), wantDeadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness, operation := newReceiverShutdownHarness(t, false)
			operation.remoteDecision = receiverAttemptDecision{
				transitionOwner:       ReceiverTerminalRemote,
				transitionProvenance:  ReceiverProvenanceRemoteFailureScopeViolation,
				disposition:           ReceiverDispositionSessionUnsafe,
				consequenceProvenance: ReceiverProvenanceRemoteFailureScopeViolation,
			}
			operation.remoteError <- test.cause
			receiveTest(t, harness.attempt.Done())

			outcome := harness.attempt.Outcome()
			if outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
				outcome.TransitionAuthority() != ReceiverTerminalRemote ||
				outcome.TransitionProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
				outcome.ConsequenceProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
				!outcome.RequiresSessionClose() ||
				!outcome.DiagnosticsTruncated() ||
				!outcome.HasRetainedCauseClass(ReceiverCauseProtocol) ||
				!outcome.HasRetainedCauseClass(ReceiverCauseUnknown) {
				t.Fatalf("structurally scoped receiver outcome=%+v", outcome)
			}
			if outcome.HasRetainedCauseClass(ReceiverCauseDeadline) != test.wantDeadline {
				t.Fatalf("deadline diagnostic=%t, want %t: %+v",
					outcome.HasRetainedCauseClass(ReceiverCauseDeadline), test.wantDeadline, outcome)
			}
			wantClasses := []ReceiverCauseClass{
				ReceiverCauseProtocol,
			}
			if test.wantDeadline {
				wantClasses = append(wantClasses, ReceiverCauseDeadline)
			}
			wantClasses = append(wantClasses, ReceiverCauseUnknown)
			if !reflect.DeepEqual(outcome.RetainedCauseClasses(), wantClasses) {
				t.Fatalf("canonical structural classes=%v, want %v", outcome.RetainedCauseClasses(), wantClasses)
			}
		})
	}
}

func TestReceiverAttemptTruncatedDiagnosticsDoNotInventSessionAuthority(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	children := make([]error, maximumReceiverErrorTreeNodes+1)
	for index := range children {
		children[index] = ErrProtocol
	}
	operation.remoteError <- errors.Join(children...)
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		outcome.TransitionAuthority() != ReceiverTerminalRemote ||
		outcome.TransitionProvenance() != ReceiverProvenanceRemoteOperationRejected ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceRemoteOperationRejected ||
		outcome.RequiresSessionClose() ||
		!outcome.DiagnosticsTruncated() ||
		!outcome.HasRetainedCauseClass(ReceiverCauseUnknown) {
		t.Fatalf("truncated attempt-scoped outcome=%+v", outcome)
	}
}

func TestReceiverTerminationTraceContainsSafeCorrelationAndClassification(t *testing.T) {
	traces := make(chan ReceiverTerminationTrace, 1)
	var operation *exactReceiverTestOperation
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		config.OnTermination = func(trace ReceiverTerminationTrace) { traces <- trace }
		operation = newExactReceiverTestOperation(
			signaling.operation.(*receiverTestOperation),
			false,
		)
		signaling.operation = operation
	})
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
	firstOperation.remoteError <- sessionruntime.ErrOperationMissing
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
