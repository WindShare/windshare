package receiversession

import (
	"bytes"
	"context"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestReceiverPeerOpenNormalizesNilContextAndRequiresContinuationContract(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		runtime, _ := newUnstartedRuntimeWithContinuations(
			t,
			protocolsession.RoleReceiver,
			protocolsession.OperationLimits{},
			nil,
			continuationReplayClassifier{},
		)
		startReceiverPeerWriter(t, runtime)
		rpc := newTestRequests(
			runtime,
			bytes.NewReader(bytes.Repeat([]byte{0xc1}, protocolsession.IdentityBytes)),
		)
		receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
		operation, err := receiver.OpenPeerOperation(receiverPeerMissingContext(), []byte{0xf6})
		if err != nil || operation == nil || operation.OperationID().IsZero() {
			t.Fatalf("nil-context peer open operation=%+v error=%v", operation, err)
		}
		termination := operation.Terminate(receiverPeerMissingContext())
		if !operation.OwnsTermination(termination) ||
			termination.TransitionProvenance() != PeerProvenanceLocalExplicitStop {
			t.Fatalf("nil-context peer termination=%+v", termination)
		}
	})

	t.Run("missing continuation classifier", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		startReceiverPeerWriter(t, runtime)
		rpc := newTestRequests(
			runtime,
			bytes.NewReader(bytes.Repeat([]byte{0xc2}, protocolsession.IdentityBytes)),
		)
		receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
		operation, err := receiver.OpenPeerOperation(context.Background(), []byte{0xf6})
		if operation != nil || err == nil || len(rpc.calls) != 0 {
			t.Fatalf("classifier-less peer open operation=%+v error=%v calls=%d", operation, err, len(rpc.calls))
		}
	})
}

func TestReceiverPeerCandidateAndReceiveStateTransitionsFailClosed(t *testing.T) {
	t.Run("nil candidate context", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc3)
		disposition, err := fixture.operation.SendCandidate(receiverPeerMissingContext(), []byte{0xf6})
		if err != nil || disposition != protocolsession.OperationDeliver {
			t.Fatalf("nil-context candidate disposition=%d error=%v", disposition, err)
		}
	})

	t.Run("stale generation candidate is dropped", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc4)
		generation, permit := fixture.call.OperationAuthority()
		if generation.IsZero() || permit.IsZero() {
			t.Fatal("peer call retained no operation generation")
		}
		cancelPendingCall(t, fixture.operation.rpc, fixture.call)
		disposition, err := fixture.operation.SendCandidate(
			context.Background(),
			[]byte{0xf6},
		)
		if err != nil || disposition != protocolsession.OperationDrop {
			t.Fatalf("stale candidate disposition=%d error=%v", disposition, err)
		}
	})

	t.Run("published terminal is returned to later receive", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc5)
		want := fixture.operation.Terminate(context.Background())
		got := requireReceiverPeerTermination(t, fixture.operation.Receive(context.Background()))
		assertReceiverPeerTerminationsEqual(t, want, got)
	})

	t.Run("second concurrent receive is rejected", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc6)
		fixture.operation.mu.Lock()
		fixture.operation.receiving = true
		fixture.operation.mu.Unlock()
		call, terminal, err := fixture.operation.beginReceive()
		if call != nil || terminal != nil || !errors.Is(err, sessioncontract.ErrOperationOverflow) {
			t.Fatalf("concurrent begin call=%+v terminal=%+v error=%v", call, terminal, err)
		}
		fixture.operation.mu.Lock()
		fixture.operation.receiving = false
		fixture.operation.mu.Unlock()
	})
}

// receiverPeerMissingContext keeps intentional normalization coverage distinct
// from accidental nil Context arguments in ordinary tests.
func receiverPeerMissingContext() context.Context { return nil }

func TestReceiverPeerEvidenceClassificationPreservesOwnershipScope(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xc7)
	if evidence, ok := receiverPeerAuthenticatedViolationEvidence(
		protocolsession.AuthenticatedOperationViolation{},
	); ok || evidence.transition.authority != receiverPeerTerminalAuthorityInvalid {
		t.Fatalf("zero authenticated violation evidence=%+v ok=%v", evidence, ok)
	}
	fixture.operation.observeAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation{})
	var nilOperation *PeerOperation
	nilOperation.observeAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation{})

	runtimeEvidence := fixture.operation.classifyReceiveError(context.Background(), sessioncontract.ErrRuntimeClosed)
	if runtimeEvidence.transition.authority != PeerTerminalAuthorityRuntime ||
		runtimeEvidence.transition.provenance != PeerProvenanceRuntimeStopping {
		t.Fatalf("runtime evidence=%+v", runtimeEvidence)
	}
	contractCause := errors.New("peer response contract failed")
	contractEvidence := fixture.operation.classifyReceiveError(context.Background(), contractCause)
	if contractEvidence.transition.authority != PeerTerminalAuthorityLocal ||
		contractEvidence.transition.provenance != PeerProvenanceLocalOperationContract {
		t.Fatalf("contract evidence=%+v", contractEvidence)
	}

	missingLifecycle := &PeerOperation{
		rpc: &sessioncore.Requests{},
	}
	if evidence, stopping := missingLifecycle.runtimeTerminalEvidence(nil); !stopping ||
		evidence.transition.authority != PeerTerminalAuthorityRuntime {
		t.Fatalf("missing-lifecycle evidence=%+v stopping=%v", evidence, stopping)
	}
	stopped, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	stopTestCore(stopped)
	finishedRuntime := &PeerOperation{
		rpc: newTestRequests(stopped, &deterministicReader{next: 27}),
	}
	if evidence, stopping := finishedRuntime.runtimeTerminalEvidence(nil); !stopping ||
		evidence.transition.authority != PeerTerminalAuthorityRuntime {
		t.Fatalf("finished-runtime evidence=%+v stopping=%v", evidence, stopping)
	}

	malformed := receiverPeerRemoteFailureEvidence(protocolsession.Message{})
	if malformed.transition.provenance != PeerProvenanceRemoteFailureMalformed ||
		malformed.consequence.severity != PeerTerminalSessionUnsafe {
		t.Fatalf("malformed remote evidence=%+v", malformed)
	}
	for _, testCase := range []struct {
		cause error
		code  PeerDiagnosticCode
	}{
		{cause: sessioncontract.ErrOperationMissing, code: PeerDiagnosticOperationMissing},
		{cause: sessioncontract.ErrRuntimeClosed, code: PeerDiagnosticRuntimeClosed},
		{cause: sessioncontract.ErrOperationOverflow, code: PeerDiagnosticOperationOverflow},
		{cause: protocolsession.ErrUnknownMessageKind, code: PeerDiagnosticUnknownControl},
	} {
		diagnostic := receiverPeerDiagnosticForCause(testCase.cause, PeerDiagnosticOpaqueFailure)
		if diagnostic.Code() != testCase.code {
			t.Fatalf("cause %v diagnostic=%d, want %d", testCase.cause, diagnostic.Code(), testCase.code)
		}
	}
}

func TestReceiverPeerTerminalPublicationContainsControlAndCleanupRaces(t *testing.T) {
	t.Run("terminal transition wins control completion", func(t *testing.T) {
		operation := &PeerOperation{
			token:        new(receiverPeerOperationToken),
			receiving:    true,
			terminalDone: make(chan struct{}),
		}
		evidence := newReceiverPeerTerminalEvidence(
			PeerTerminalAuthorityRemote,
			PeerProvenanceRemoteUnknownControl,
			PeerTerminalOperationOnly,
			receiverPeerDiagnostic(PeerDiagnosticUnknownControl),
		)
		_, claimed, _ := operation.claimTerminal(evidence)
		if !claimed {
			t.Fatal("terminal evidence did not claim operation")
		}
		operation.completeTerminalCleanup(nil)
		result := operation.completeControlReceive(PeerControl{})
		termination := requireReceiverPeerTermination(t, result)
		if !operation.OwnsTermination(termination) ||
			termination.TransitionProvenance() != PeerProvenanceRemoteUnknownControl {
			t.Fatalf("control-race termination=%+v", termination)
		}
	})

	t.Run("invalid evidence becomes local contract failure", func(t *testing.T) {
		operation := &PeerOperation{
			token:        new(receiverPeerOperationToken),
			terminalDone: make(chan struct{}),
		}
		_, claimed, done := operation.claimTerminal(receiverPeerTerminalEvidence{})
		if !claimed {
			t.Fatal("invalid evidence did not fail closed")
		}
		operation.completeTerminalCleanup(nil)
		termination := operation.awaitTerminal(done)
		if !operation.OwnsTermination(termination) ||
			termination.TransitionProvenance() != PeerProvenanceLocalOperationContract ||
			!receiverPeerDiagnosticsContain(termination.Diagnostics(), PeerDiagnosticOperationMissing) {
			t.Fatalf("invalid-evidence termination=%+v", termination)
		}
	})

	operation := &PeerOperation{}
	if err := operation.cancelExact(context.Background(), nil); err != nil {
		t.Fatalf("nil-call cancellation error=%v", err)
	}
	if validReceiverPeerTransition(receiverPeerTerminalTransition{}) {
		t.Fatal("zero terminal transition validated")
	}
	if validReceiverPeerConsequence(receiverPeerTerminalConsequence{}) {
		t.Fatal("zero terminal consequence validated")
	}
	if validReceiverPeerTransition(receiverPeerTerminalTransition{
		authority:  PeerTerminalAuthorityLocal,
		provenance: PeerProvenanceRemoteUnknownControl,
	}) {
		t.Fatal("cross-authority terminal transition validated")
	}
	if validReceiverPeerConsequence(receiverPeerTerminalConsequence{
		severity:   PeerTerminalOperationOnly,
		provenance: PeerProvenanceRemoteFailureMalformed,
	}) {
		t.Fatal("unsafe provenance validated as operation-only")
	}
}
