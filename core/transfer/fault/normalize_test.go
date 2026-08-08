package fault_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
)

func TestNormalizeBoundaryExcludesCancellationAndDefaultsUnknownErrors(t *testing.T) {
	t.Parallel()

	normalized := mustCheckpoint(t, fault.ScopeOutputPause, fault.CheckpointCorruptRecord)
	diagnostic := errors.New("native checkpoint decoder detail")
	typed := fault.Wrap(normalized, diagnostic)
	wrapped := fmt.Errorf("repository load: %w", typed)
	result := fault.NormalizeBoundary(context.Background(), wrapped)
	if actual, ok := result.Fault(); result.Kind() != fault.BoundaryFailed || !ok || actual != normalized {
		t.Fatalf("typed normalization = (%v, %v, %v), want %v", result.Kind(), actual, ok, normalized)
	}
	contextFree := fault.NormalizeBoundaryError(wrapped)
	if actual, ok := contextFree.Fault(); contextFree.Kind() != fault.BoundaryFailed || !ok || actual != normalized {
		t.Fatalf("context-free normalization = (%v, %v, %v), want %v", contextFree.Kind(), actual, ok, normalized)
	}
	if !errors.Is(typed, diagnostic) {
		t.Fatal("typed boundary error lost immediate diagnostic cause")
	}
	var boundaryFailure *fault.BoundaryError
	if !errors.As(typed, &boundaryFailure) || boundaryFailure.Fault() != normalized || boundaryFailure.Error() == "" {
		t.Fatal("typed boundary error did not expose its immediate diagnostic projection")
	}
	withoutCause := fault.Wrap(normalized, nil)
	if withoutCause.Error() == "" {
		t.Fatal("boundary error without a native cause had no diagnostic message")
	}

	unknown := fault.NormalizeBoundary(context.Background(), errors.New("untyped collaborator failure"))
	unknownFault, ok := unknown.Fault()
	if !ok || unknownFault != fault.DependencyContractFault() ||
		unknownFault.Domain() != fault.DomainSession || unknownFault.Scope() != fault.ScopeOutputPause {
		t.Fatalf("unknown normalization = (%v, %v)", unknownFault, ok)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if result := fault.NormalizeBoundary(canceled, typed); result.Kind() != fault.BoundaryCanceled {
		t.Fatalf("canceled context result = %v", result.Kind())
	}
	if result := fault.NormalizeBoundary(context.Background(), fault.Wrap(normalized, context.Canceled)); result.Kind() != fault.BoundaryCanceled {
		t.Fatalf("wrapped cancellation result = %v", result.Kind())
	}
	if result := fault.NormalizeBoundary(context.Background(), context.DeadlineExceeded); result.Kind() != fault.BoundaryCanceled {
		t.Fatalf("deadline result = %v", result.Kind())
	}
	if result := fault.NormalizeBoundary(canceled, nil); result.Kind() != fault.BoundaryCanceled {
		t.Fatalf("cancellation-first result = %v", result.Kind())
	}
	if result := fault.NormalizeBoundary(context.Background(), nil); result.Kind() != fault.BoundarySuccess {
		t.Fatalf("successful result = %v", result.Kind())
	}

	var typedNil *fault.BoundaryError
	if result := fault.NormalizeBoundary(context.Background(), typedNil); result.Kind() != fault.BoundaryFailed {
		t.Fatalf("typed nil result = %v", result.Kind())
	}
	if typedNil.Error() != fault.ErrInvalidFault.Error() || typedNil.Unwrap() != nil || typedNil.Fault() != (fault.Fault{}) {
		t.Fatal("typed nil boundary error was not fail-closed")
	}
	if err := fault.Wrap(fault.Fault{}, diagnostic); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid wrap error = %v", err)
	}
}

func TestReduceBoundaryErrorsJoinsSiblingFaultsWithoutAbsorbingCancellation(t *testing.T) {
	t.Parallel()

	fileLocal := mustSource(t, fault.ScopeFileLocal, fault.SourcePermanent)
	terminal, err := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionProtocol)
	if err != nil {
		t.Fatal(err)
	}
	left := fault.Wrap(fileLocal, errors.New("source"))
	right := fault.Wrap(terminal, errors.New("session"))
	for _, candidates := range [][]error{{left, right}, {right, left}} {
		reduced := fault.ReduceBoundaryErrors(context.Background(), candidates...)
		result := fault.NormalizeBoundary(context.Background(), reduced)
		actual, ok := result.Fault()
		if !ok || actual != terminal {
			t.Fatalf("reduced fault = (%v, %v), want %v", actual, ok, terminal)
		}
		contextFree := fault.ReduceBoundaryErrorSet(candidates...)
		contextFreeResult := fault.NormalizeBoundaryError(contextFree)
		contextFreeFault, contextFreeOK := contextFreeResult.Fault()
		if !contextFreeOK || contextFreeFault != terminal {
			t.Fatalf("context-free reduced fault = (%v, %v), want %v", contextFreeFault, contextFreeOK, terminal)
		}
	}
	if reduced := fault.ReduceBoundaryErrors(context.Background(), left, context.DeadlineExceeded, right); !errors.Is(reduced, context.DeadlineExceeded) {
		t.Fatalf("reduced cancellation = %v", reduced)
	}
	if reduced := fault.ReduceBoundaryErrors(context.Background(), nil, nil); reduced != nil {
		t.Fatalf("empty reduction = %v", reduced)
	}
}

func TestRetirementRequiresExplicitFileLocalSourceCode(t *testing.T) {
	t.Parallel()

	permanent := mustSource(t, fault.ScopeFileLocal, fault.SourcePermanent)
	if reason, ok := fault.RetirementFor(permanent); !ok || reason != fault.RetirementPermanentSource {
		t.Fatalf("permanent retirement = (%v, %v)", reason, ok)
	}
	invalidated := mustSource(t, fault.ScopeFileLocal, fault.SourceRevisionInvalidated)
	if reason, ok := fault.RetirementFor(invalidated); !ok || reason != fault.RetirementInvalidatedRevision {
		t.Fatalf("invalidated retirement = (%v, %v)", reason, ok)
	}

	notAuthorized := []fault.Fault{
		mustSource(t, fault.ScopeFileLocal, fault.SourceUnavailable),
		mustSource(t, fault.ScopeFileLocal, fault.SourceRevisionChanged),
		mustSource(t, fault.ScopeOutputPause, fault.SourcePermanent),
		fault.DependencyContractFault(),
		{},
	}
	for _, candidate := range notAuthorized {
		if reason, ok := fault.RetirementFor(candidate); ok || reason != 0 {
			t.Fatalf("fault %v authorized retirement (%v, %v)", candidate, reason, ok)
		}
	}
}
