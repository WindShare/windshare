package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverPeerOpenNormalizesNilContextAndRequiresContinuationAuthority(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		runtime, _ := newUnstartedRuntimeWithContinuations(
			t,
			protocolsession.RoleReceiver,
			protocolsession.OperationLimits{},
			nil,
			continuationReplayClassifier{},
		)
		startReceiverPeerWriter(t, runtime)
		rpc := newRPCClient(
			runtime,
			bytes.NewReader(bytes.Repeat([]byte{0xc1}, protocolsession.IdentityBytes)),
		)
		receiver := &ReceiverRuntime{runtimeCore: runtime, rpc: rpc}
		operation, err := receiver.OpenPeerOperation(nil, []byte{0xf6})
		if err != nil || operation == nil || operation.OperationID().IsZero() {
			t.Fatalf("nil-context peer open operation=%+v error=%v", operation, err)
		}
		termination := operation.Terminate(nil)
		if !operation.OwnsTermination(termination) ||
			termination.TransitionProvenance() != ReceiverPeerProvenanceLocalExplicitStop {
			t.Fatalf("nil-context peer termination=%+v", termination)
		}
	})

	t.Run("missing continuation classifier", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		startReceiverPeerWriter(t, runtime)
		rpc := newRPCClient(
			runtime,
			bytes.NewReader(bytes.Repeat([]byte{0xc2}, protocolsession.IdentityBytes)),
		)
		receiver := &ReceiverRuntime{runtimeCore: runtime, rpc: rpc}
		operation, err := receiver.OpenPeerOperation(context.Background(), []byte{0xf6})
		if operation != nil || err == nil || len(rpc.calls) != 0 {
			t.Fatalf("classifier-less peer open operation=%+v error=%v calls=%d", operation, err, len(rpc.calls))
		}
	})
}

func TestReceiverPeerCandidateAndReceiveStateTransitionsFailClosed(t *testing.T) {
	t.Run("nil candidate context", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc3)
		disposition, err := fixture.operation.SendCandidate(nil, []byte{0xf6})
		if err != nil || disposition != protocolsession.OperationDeliver {
			t.Fatalf("nil-context candidate disposition=%d error=%v", disposition, err)
		}
	})

	t.Run("stale generation candidate is dropped", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xc4)
		generation, permit := fixture.call.operationAuthority()
		if generation.IsZero() || permit.IsZero() {
			t.Fatal("peer call retained no operation generation")
		}
		if err := fixture.runtime.operations.CancelGeneration(generation); err != nil {
			t.Fatal(err)
		}
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
		if call != nil || terminal != nil || !errors.Is(err, ErrOperationOverflow) {
			t.Fatalf("concurrent begin call=%+v terminal=%+v error=%v", call, terminal, err)
		}
		fixture.operation.mu.Lock()
		fixture.operation.receiving = false
		fixture.operation.mu.Unlock()
	})
}

func TestReceiverPeerEvidenceClassificationPreservesOwnershipScope(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xc7)
	if evidence, ok := receiverPeerAuthenticatedViolationEvidence(
		protocolsession.AuthenticatedOperationViolation{},
	); ok || evidence.transition.authority != receiverPeerTerminalAuthorityInvalid {
		t.Fatalf("zero authenticated violation evidence=%+v ok=%v", evidence, ok)
	}
	fixture.operation.observeAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation{})
	var nilOperation *ReceiverPeerOperation
	nilOperation.observeAuthenticatedOperationViolation(protocolsession.AuthenticatedOperationViolation{})

	runtimeEvidence := fixture.operation.classifyReceiveError(context.Background(), ErrRuntimeClosed)
	if runtimeEvidence.transition.authority != ReceiverPeerTerminalAuthorityRuntime ||
		runtimeEvidence.transition.provenance != ReceiverPeerProvenanceRuntimeStopping {
		t.Fatalf("runtime evidence=%+v", runtimeEvidence)
	}
	contractCause := errors.New("peer response contract failed")
	contractEvidence := fixture.operation.classifyReceiveError(context.Background(), contractCause)
	if contractEvidence.transition.authority != ReceiverPeerTerminalAuthorityLocal ||
		contractEvidence.transition.provenance != ReceiverPeerProvenanceLocalOperationContract {
		t.Fatalf("contract evidence=%+v", contractEvidence)
	}

	missingLifecycle := &ReceiverPeerOperation{
		rpc: &rpcClient{runtime: &runtimeCore{ctx: context.Background()}},
	}
	if evidence, stopping := missingLifecycle.runtimeTerminalEvidence(nil); !stopping ||
		evidence.transition.authority != ReceiverPeerTerminalAuthorityRuntime {
		t.Fatalf("missing-lifecycle evidence=%+v stopping=%v", evidence, stopping)
	}
	done := make(chan struct{})
	close(done)
	finishedRuntime := &ReceiverPeerOperation{
		rpc: &rpcClient{runtime: &runtimeCore{ctx: context.Background(), done: done}},
	}
	if evidence, stopping := finishedRuntime.runtimeTerminalEvidence(nil); !stopping ||
		evidence.transition.authority != ReceiverPeerTerminalAuthorityRuntime {
		t.Fatalf("finished-runtime evidence=%+v stopping=%v", evidence, stopping)
	}

	malformed := receiverPeerRemoteFailureEvidence(protocolsession.Message{})
	if malformed.transition.provenance != ReceiverPeerProvenanceRemoteFailureMalformed ||
		malformed.consequence.severity != ReceiverPeerTerminalSessionUnsafe {
		t.Fatalf("malformed remote evidence=%+v", malformed)
	}
	for _, testCase := range []struct {
		cause error
		code  ReceiverPeerDiagnosticCode
	}{
		{cause: ErrOperationMissing, code: ReceiverPeerDiagnosticOperationMissing},
		{cause: ErrRuntimeClosed, code: ReceiverPeerDiagnosticRuntimeClosed},
		{cause: ErrOperationOverflow, code: ReceiverPeerDiagnosticOperationOverflow},
		{cause: protocolsession.ErrUnknownMessageKind, code: ReceiverPeerDiagnosticUnknownControl},
	} {
		diagnostic := receiverPeerDiagnosticForCause(testCase.cause, ReceiverPeerDiagnosticOpaqueFailure)
		if diagnostic.Code() != testCase.code {
			t.Fatalf("cause %v diagnostic=%d, want %d", testCase.cause, diagnostic.Code(), testCase.code)
		}
	}
}

func TestReceiverPeerTerminalPublicationContainsControlAndCleanupRaces(t *testing.T) {
	t.Run("terminal transition wins control completion", func(t *testing.T) {
		operation := &ReceiverPeerOperation{
			token:        new(receiverPeerOperationToken),
			receiving:    true,
			terminalDone: make(chan struct{}),
		}
		evidence := newReceiverPeerTerminalEvidence(
			ReceiverPeerTerminalAuthorityRemote,
			ReceiverPeerProvenanceRemoteUnknownControl,
			ReceiverPeerTerminalOperationOnly,
			receiverPeerDiagnostic(ReceiverPeerDiagnosticUnknownControl),
		)
		_, claimed, _ := operation.claimTerminal(evidence)
		if !claimed {
			t.Fatal("terminal evidence did not claim operation")
		}
		operation.completeTerminalCleanup(nil)
		result := operation.completeControlReceive(ReceiverPeerControl{})
		termination := requireReceiverPeerTermination(t, result)
		if !operation.OwnsTermination(termination) ||
			termination.TransitionProvenance() != ReceiverPeerProvenanceRemoteUnknownControl {
			t.Fatalf("control-race termination=%+v", termination)
		}
	})

	t.Run("invalid evidence becomes local contract failure", func(t *testing.T) {
		operation := &ReceiverPeerOperation{
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
			termination.TransitionProvenance() != ReceiverPeerProvenanceLocalOperationContract ||
			!receiverPeerDiagnosticsContain(termination.Diagnostics(), ReceiverPeerDiagnosticOperationMissing) {
			t.Fatalf("invalid-evidence termination=%+v", termination)
		}
	})

	operation := &ReceiverPeerOperation{}
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
		authority:  ReceiverPeerTerminalAuthorityLocal,
		provenance: ReceiverPeerProvenanceRemoteUnknownControl,
	}) {
		t.Fatal("cross-authority terminal transition validated")
	}
	if validReceiverPeerConsequence(receiverPeerTerminalConsequence{
		severity:   ReceiverPeerTerminalOperationOnly,
		provenance: ReceiverPeerProvenanceRemoteFailureMalformed,
	}) {
		t.Fatal("unsafe provenance validated as operation-only")
	}
}

func startReceiverPeerWriter(t *testing.T, runtime *runtimeCore) {
	t.Helper()
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lane.writer.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}
