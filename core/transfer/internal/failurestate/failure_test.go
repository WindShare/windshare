package failurestate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
)

func TestNormalizationPreservesOwnerCauseWithoutReopeningDiagnostics(t *testing.T) {
	value, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceRevisionChanged)
	diagnostic := errors.New("revision changed")
	owned := failurestate.New(value, diagnostic)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(owned)
	got := failurestate.Normalize(ctx, context.Canceled, nil)
	if got != owned || got.Canceled() || got.Fault() != value {
		t.Fatalf("owner cause lost its provenance: %+v", got)
	}
	if errors.Is(got, diagnostic) || errors.Unwrap(got) != nil {
		t.Fatal("normalized diagnostics escaped into policy matching")
	}
	if failurestate.Diagnostic(got) != diagnostic || got.Diagnostic() != diagnostic {
		t.Fatal("diagnostic evidence was discarded")
	}
	if admitted, ok := failurestate.Admit(fmt.Errorf("wrapped: %w", owned)); ok || admitted != nil {
		t.Fatal("a wrapper acquired closed internal authority")
	}
	raw := errors.New("unclassified collaborator error")
	if failurestate.Require(raw).Fault() != fault.DependencyContractFault() {
		t.Fatal("unknown internal error acquired source authority")
	}
	if failurestate.Require(nil) != nil || failurestate.Err(nil) != nil || failurestate.ContextCause(context.Background()) != nil {
		t.Fatal("absence of a failure became a typed-nil error")
	}
}

func TestSiblingReductionPreservesFaultAndCancellationIndependently(t *testing.T) {
	sourceValue, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourcePermanent)
	sessionValue, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionProtocol)
	source := failurestate.New(sourceValue, errors.New("source"))
	session := failurestate.New(sessionValue, errors.New("session"))
	canceled := failurestate.FromCancellation(context.Background(), context.Canceled)
	deadline := failurestate.FromCancellation(context.Background(), context.DeadlineExceeded)

	for _, candidates := range [][]*failurestate.Failure{
		{source, session, canceled, deadline}, {deadline, canceled, session, source},
	} {
		result := failurestate.JoinClosed(candidates...)
		if result.Fault() != sessionValue || result.Cancellation() != context.DeadlineExceeded ||
			!errors.Is(result, context.DeadlineExceeded) || errors.Is(result, context.Canceled) {
			t.Fatalf("sibling order changed the outcome: %v", result)
		}
		if failurestate.BoundaryError(result, result.Diagnostic()) != context.DeadlineExceeded {
			t.Fatal("cancellation was lost when returning across the collaborator boundary")
		}
	}
	if failurestate.Reduce(nil, source) != source || failurestate.Merge(nil, source) != source ||
		failurestate.Merge(source, nil) != source {
		t.Fatal("singleton reduction replaced an owned cause")
	}
	if result := failurestate.Merge(source, session); result.Fault() != sessionValue {
		t.Fatalf("merged fault = %v", result)
	}
	if failurestate.Join(nil) != nil {
		t.Fatal("empty sibling set produced a failure")
	}
	wrappedCancellation := fmt.Errorf("diagnostic wrapper: %w", context.Canceled)
	if result := failurestate.Reduce(wrappedCancellation); result.Canceled() || result.Fault() != fault.DependencyContractFault() {
		t.Fatal("an unadmitted error graph acquired internal cancellation authority")
	}
	if result := failurestate.Normalize(context.Background(), wrappedCancellation, nil); !result.Canceled() {
		t.Fatal("the immediate boundary rejected collaborator cancellation")
	}
}

func TestSourceClassificationIsSharedAcrossLaneAndJobBoundaries(t *testing.T) {
	cases := []struct {
		cause  error
		scope  fault.Scope
		source fault.SourceCode
	}{
		{content.ErrRevisionDrift, fault.ScopeFileLocal, fault.SourceRevisionInvalidated},
		{content.ErrRevisionStale, fault.ScopeFileLocal, fault.SourceRevisionChanged},
		{content.ErrRevisionNotFound, fault.ScopeFileLocal, fault.SourcePermanent},
		{content.ErrLeaseExpired, fault.ScopeFileLocal, fault.SourceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.cause.Error(), func(t *testing.T) {
			got := failurestate.Normalize(context.Background(), fmt.Errorf("source: %w", tc.cause), failurestate.ContentFault)
			value, _ := fault.NewSource(tc.scope, tc.source)
			if got.Fault() != value || got.Canceled() {
				t.Fatalf("source classification = %v, want %v", got, value)
			}
		})
	}
	if _, known := failurestate.ContentFault(protocolsession.ErrSessionTerminated); known ||
		!failurestate.SessionTerminal(protocolsession.ErrWriterStopped) {
		t.Fatal("content classification absorbed transport teardown policy")
	}
	unknown := errors.New("unknown source error")
	if got := failurestate.Normalize(context.Background(), unknown, failurestate.ContentFault); got.Fault() != fault.DependencyContractFault() {
		t.Fatalf("unknown failure escaped the contract boundary: %v", got)
	}
	if failurestate.Normalize(context.Background(), nil, failurestate.ContentFault) != nil {
		t.Fatal("successful source result acquired a failure")
	}
}

func TestTypedBoundaryAndCancellationAdmissionRemainDistinct(t *testing.T) {
	value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionResourceBudget)
	diagnostic := errors.New("budget")
	boundary := fault.Wrap(value, diagnostic)
	normalized := failurestate.Normalize(context.Background(), boundary, nil)
	if normalized.Fault() != value || failurestate.FaultOf(normalized) != value {
		t.Fatal("typed collaborator fault was not admitted")
	}
	returned := failurestate.BoundaryError(normalized, diagnostic)
	var typed *fault.BoundaryError
	if !errors.As(returned, &typed) || typed.Fault() != value || !errors.Is(returned, diagnostic) {
		t.Fatalf("boundary projection lost diagnostic or policy: %v", returned)
	}
	if failurestate.FaultOf(boundary).Valid() {
		t.Fatal("raw boundary value was treated as closed internal policy")
	}
	if failurestate.FromCancellation(context.Background(), normalized) != normalized {
		t.Fatal("owned failure was replaced during cancellation projection")
	}
	if got := failurestate.FromCancellation(context.Background(), diagnostic); got.Fault() != fault.DependencyContractFault() {
		t.Fatal("an arbitrary diagnostic was treated as cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !failurestate.IsCancellation(ctx, nil) || !failurestate.Normalize(ctx, boundary, nil).Canceled() {
		t.Fatal("caller cancellation lost to a collaborator fault")
	}
	if failurestate.IsCancellation(context.Background(), diagnostic) {
		t.Fatal("an ordinary error was treated as cancellation")
	}
}
