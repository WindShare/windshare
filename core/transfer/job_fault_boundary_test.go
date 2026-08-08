package transfer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
)

func TestCollaboratorBoundaryNormalizationIsClosed(t *testing.T) {
	t.Parallel()

	typedValue, err := fault.NewCheckpoint(fault.ScopeOutputPause, fault.CheckpointCorruptRecord)
	if err != nil {
		t.Fatal(err)
	}
	typed := fault.Wrap(typedValue, errors.New("checkpoint decoder detail"))
	if got := normalizedFault(normalizeOutputBoundary(context.Background(), typed)); got != typedValue {
		t.Fatalf("typed boundary fault = %v, want %v", got, typedValue)
	}

	unknown := normalizeOutputBoundary(context.Background(), errors.New("untyped backend failure"))
	if got := normalizedFault(unknown); got != fault.DependencyContractFault() {
		t.Fatalf("unknown boundary fault = %v", got)
	}
	if !isJobTerminalError(unknown) {
		t.Fatal("unknown collaborator failure did not pause the output session")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cancellation := normalizeOutputBoundary(canceled, typed)
	policy := lifecyclePolicyFor(cancellation)
	if !policy.canceled || policy.value.Valid() || !errors.Is(cancellation, context.Canceled) {
		t.Fatalf("cancellation policy = %+v, err=%v", policy, cancellation)
	}
}

func TestLifecycleFailureJoinUsesOnlyNormalizedSeverity(t *testing.T) {
	t.Parallel()

	file := sourcePermanentFailure(errors.New("permanent source failure"))
	pause := outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("output state failure"))
	terminal := sessionProtocolFailure(errors.New("authenticated protocol failure"))

	joined := joinLifecycleFailures(file, pause, terminal)
	if got := normalizedFault(joined); got != normalizedFault(terminal) {
		t.Fatalf("joined fault = %v, want %v", got, normalizedFault(terminal))
	}
	if got := normalizedFault(joinLifecycleFailures(terminal, file, pause)); got != normalizedFault(terminal) {
		t.Fatalf("permuted joined fault = %v, want %v", got, normalizedFault(terminal))
	}

	withCancellation := joinLifecycleFailures(cancellationFailure(canceledContext(), context.Canceled), pause)
	policy := lifecyclePolicyFor(withCancellation)
	if !policy.canceled || policy.value != normalizedFault(pause) {
		t.Fatalf("joined cancellation policy = %+v", policy)
	}
}

func TestRetirementAndIsolationRequireExplicitClosedFaults(t *testing.T) {
	t.Parallel()

	permanent := sourcePermanentFailure(errors.New("permanent"))
	if got := fileRetireReason(permanent); got != FileRetireIsolatedPermanentSourceFailure {
		t.Fatalf("permanent retire reason = %v", got)
	}
	if got := fileRetireReason(dependencyContractFailure(errors.New("unknown"))); got != 0 {
		t.Fatalf("dependency-contract fault authorized retirement: %v", got)
	}

	fileOutput := outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, errors.New("one file"))
	isolating := OutputCapabilities{FileFailureIsolation: true}
	if !lifecyclePolicyFor(fileOutput).outputCanContinueAfterFileSettlement(isolating) {
		t.Fatal("explicit file-local output fault was not isolated")
	}
	if lifecyclePolicyFor(dependencyContractFailure(errors.New("unknown"))).outputCanContinueAfterFileSettlement(isolating) {
		t.Fatal("unknown fault acquired file-isolation authority")
	}
}

func TestLifecycleAuthorityRejectsWrappedInternalCarriers(t *testing.T) {
	t.Parallel()

	direct := sourcePermanentFailure(errors.New("permanent source diagnostic"))
	wrapped := fmt.Errorf("untrusted wrapper: %w", direct)
	if got := normalizedFault(wrapped); got != fault.DependencyContractFault() {
		t.Fatalf("wrapped internal carrier fault = %v", got)
	}
	if reason := fileRetireReason(wrapped); reason != 0 {
		t.Fatalf("wrapped internal carrier authorized retirement: %v", reason)
	}

	wrappedCancellation := fmt.Errorf("untrusted wrapper: %w", context.Canceled)
	joined := joinLifecycleFailures(wrappedCancellation)
	policy := lifecyclePolicyFor(joined)
	if policy.canceled || policy.value != fault.DependencyContractFault() || errors.Is(joined, context.Canceled) {
		t.Fatalf("wrapped cancellation acquired authority: policy=%+v err=%v", policy, joined)
	}

	directCancellation := joinLifecycleFailures(context.Canceled)
	if policy := lifecyclePolicyFor(directCancellation); !policy.canceled || policy.value.Valid() ||
		!errors.Is(directCancellation, context.Canceled) {
		t.Fatalf("direct cancellation was not preserved: policy=%+v err=%v", policy, directCancellation)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
