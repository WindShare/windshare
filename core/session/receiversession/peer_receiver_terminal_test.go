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

func TestReceiverPeerTerminalConsequenceDominanceIsIndependentOfTransitionOrder(t *testing.T) {
	local := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityLocal,
		PeerProvenanceLocalOperationContract,
		PeerTerminalOperationOnly,
		receiverPeerDiagnostic(PeerDiagnosticOperationOverflow),
	)
	unsafeRemote := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityRemote,
		PeerProvenanceRemoteFailureScopeViolation,
		PeerTerminalSessionUnsafe,
		receiverPeerDiagnostic(PeerDiagnosticRemoteFailureScopeViolation),
	)

	for _, test := range []struct {
		name           string
		first          receiverPeerTerminalEvidence
		second         receiverPeerTerminalEvidence
		wantAuthority  PeerTerminalAuthority
		wantTransition PeerTerminalProvenance
	}{
		{
			name:           "local transition precedes unsafe remote consequence",
			first:          local,
			second:         unsafeRemote,
			wantAuthority:  PeerTerminalAuthorityLocal,
			wantTransition: PeerProvenanceLocalOperationContract,
		},
		{
			name:           "unsafe remote transition precedes local diagnostic",
			first:          unsafeRemote,
			second:         local,
			wantAuthority:  PeerTerminalAuthorityRemote,
			wantTransition: PeerProvenanceRemoteFailureScopeViolation,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := &PeerOperation{
				token:        new(receiverPeerOperationToken),
				receiving:    true,
				terminalDone: make(chan struct{}),
			}
			_, _, done := operation.claimTerminal(test.first)
			operation.claimTerminal(test.second)
			operation.completeTerminalCleanup(nil)
			operation.endReceive()
			termination := operation.awaitTerminal(done)

			assertReceiverPeerTermination(
				t,
				operation,
				termination,
				test.wantAuthority,
				test.wantTransition,
				PeerTerminalSessionUnsafe,
				PeerProvenanceRemoteFailureScopeViolation,
				PeerDiagnosticOperationOverflow,
				PeerDiagnosticRemoteFailureScopeViolation,
			)
		})
	}
}

func TestReceiverPeerTerminalRaceRetainsAuthenticatedProtocolDiagnostic(t *testing.T) {
	t.Run("local transition owns before remote failure", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb1)
		operationID := fixture.operation.OperationID()
		received := make(chan PeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "active receive", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})

		// Holding cancellation at the consumer port leaves the response sink alive after
		// local transition ownership is committed, forcing the losing remote fact to join.
		cleanupGate := make(chan struct{})
		trackedRequests(fixture.operation.rpc).cancelGate = cleanupGate
		laneLocked := true
		t.Cleanup(func() {
			if laneLocked {
				close(cleanupGate)
			}
		})
		terminated := make(chan PeerTermination, 1)
		go func() { terminated <- fixture.operation.Terminate(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "local terminal ownership", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			_ bool,
		) bool {
			return transition.authority == PeerTerminalAuthorityLocal
		})
		enqueueUnexpectedPeerResponse(t, fixture.call)
		waitReceiverPeerState(t, fixture.operation, "losing remote protocol diagnostic", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			diagnostics PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return transition.authority == PeerTerminalAuthorityLocal && !receiving &&
				receiverPeerDiagnosticsContain(diagnostics, PeerDiagnosticUnknownControl)
		})
		close(cleanupGate)
		laneLocked = false

		receiveTermination := requireReceiverPeerTermination(t, <-received)
		termination := <-terminated
		for _, observed := range []PeerTermination{receiveTermination, termination} {
			assertReceiverPeerTermination(
				t,
				fixture.operation,
				observed,
				PeerTerminalAuthorityLocal,
				PeerProvenanceLocalExplicitStop,
				PeerTerminalOperationOnly,
				PeerProvenanceLocalExplicitStop,
				PeerDiagnosticUnknownControl,
			)
		}
		if fixture.operation.OperationID() != operationID {
			t.Fatal("local-owned terminal transition changed its stable operation identity")
		}
	})

	t.Run("remote failure owns before local join", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb2)
		operationID := fixture.operation.OperationID()
		received := make(chan PeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "active receive", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})

		cleanupGate := make(chan struct{})
		trackedRequests(fixture.operation.rpc).cancelGate = cleanupGate
		laneLocked := true
		t.Cleanup(func() {
			if laneLocked {
				close(cleanupGate)
			}
		})
		enqueueUnexpectedPeerResponse(t, fixture.call)
		waitReceiverPeerState(t, fixture.operation, "remote terminal ownership", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			diagnostics PeerDiagnosticSnapshot,
			_ bool,
		) bool {
			return transition.authority == PeerTerminalAuthorityRemote &&
				receiverPeerDiagnosticsContain(diagnostics, PeerDiagnosticUnknownControl)
		})
		terminated := make(chan PeerTermination, 1)
		go func() { terminated <- fixture.operation.Terminate(context.Background()) }()
		select {
		case premature := <-terminated:
			t.Fatalf("local join returned before exact remote cleanup: %+v", premature)
		default:
		}
		close(cleanupGate)
		laneLocked = false

		receiveTermination := requireReceiverPeerTermination(t, <-received)
		termination := <-terminated
		for _, observed := range []PeerTermination{receiveTermination, termination} {
			assertReceiverPeerTermination(
				t,
				fixture.operation,
				observed,
				PeerTerminalAuthorityRemote,
				PeerProvenanceRemoteUnknownControl,
				PeerTerminalOperationOnly,
				PeerProvenanceRemoteUnknownControl,
				PeerDiagnosticUnknownControl,
			)
		}
		if fixture.operation.OperationID() != operationID {
			t.Fatal("remote-owned terminal transition changed its stable operation identity")
		}
	})
}

func TestReceiverPeerPublishedTerminalOutcomeIsImmutable(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xba)
	fixture.operation.mu.Lock()
	fixture.operation.receiving = true
	fixture.operation.mu.Unlock()

	remote := newReceiverPeerTerminalEvidence(
		PeerTerminalAuthorityRemote,
		PeerProvenanceRemoteUnknownControl,
		PeerTerminalOperationOnly,
		receiverPeerDiagnostic(PeerDiagnosticUnknownControl),
	)
	published := requireReceiverPeerTermination(
		t,
		fixture.operation.terminateFromReceive(fixture.call, remote),
	)
	lateRuntime := receiverPeerRuntimeEvidence(errors.New("late runtime claimant"))
	late := requireReceiverPeerTermination(
		t,
		fixture.operation.terminateWithoutReceive(lateRuntime, nil),
	)

	assertReceiverPeerTerminationsEqual(t, published, late)
	assertReceiverPeerTermination(
		t,
		fixture.operation,
		published,
		PeerTerminalAuthorityRemote,
		PeerProvenanceRemoteUnknownControl,
		PeerTerminalOperationOnly,
		PeerProvenanceRemoteUnknownControl,
		PeerDiagnosticUnknownControl,
	)
	foreign := &PeerOperation{token: new(receiverPeerOperationToken)}
	if foreign.OwnsTermination(published) {
		t.Fatal("a different operation accepted the published terminal capability")
	}
	components := published.Diagnostics().Components()
	components[0] = PeerDiagnostic{}
	if !receiverPeerDiagnosticsContain(published.Diagnostics(), PeerDiagnosticUnknownControl) {
		t.Fatal("mutating a diagnostic slice changed the published terminal snapshot")
	}
	if receiverPeerDiagnosticsContain(late.Diagnostics(), PeerDiagnosticRuntimeClosed) ||
		receiverPeerDiagnosticsContain(late.Diagnostics(), PeerDiagnosticOpaqueFailure) {
		t.Fatalf("published terminal retained late claimant diagnostics: %+v", late.Diagnostics().Components())
	}
}

func TestReceiverPeerReceiveRetainsCancellationDiagnostics(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xb3)
	operationID := fixture.operation.OperationID()
	generation, _ := fixture.call.OperationAuthority()
	trackedRequests(fixture.operation.rpc).cancelErr = sessioncore.ErrLaneUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	termination := requireReceiverPeerTermination(t, fixture.operation.Receive(ctx))
	assertReceiverPeerTermination(
		t,
		fixture.operation,
		termination,
		PeerTerminalAuthorityLocal,
		PeerProvenanceLocalContextEnded,
		PeerTerminalOperationOnly,
		PeerProvenanceLocalContextEnded,
		PeerDiagnosticContextCanceled,
		PeerDiagnosticCleanupFailed,
	)
	if fixture.operation.OperationID() != operationID {
		t.Fatal("failed local cancellation changed its stable operation identity")
	}
	if generation.IsActive() {
		t.Fatal("local cancellation retained operation authority")
	}
	repeated := fixture.operation.Terminate(context.Background())
	assertReceiverPeerTerminationsEqual(t, termination, repeated)
}

func TestReceiverPeerRuntimeShutdownOwnsTerminalOutcome(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xb4)
	operationID := fixture.operation.OperationID()
	received := make(chan PeerReceiveResult, 1)
	go func() { received <- fixture.operation.Receive(context.Background()) }()
	waitReceiverPeerState(t, fixture.operation, "receive before runtime shutdown", func(
		_ receiverPeerTerminalTransition,
		_ receiverPeerTerminalConsequence,
		_ PeerDiagnosticSnapshot,
		receiving bool,
	) bool {
		return receiving
	})

	stopTestCore(fixture.runtime)
	termination := requireReceiverPeerTermination(t, <-received)
	assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)
	if fixture.operation.OperationID() != operationID {
		t.Fatal("runtime shutdown changed its stable operation identity")
	}
	assertReceiverPeerTerminationsEqual(t, termination, fixture.operation.Terminate(context.Background()))
}

func TestReceiverPeerRuntimeLifecycleOwnsBeforeDonePublication(t *testing.T) {
	t.Run("receive observes call close inside finalization", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb7)
		finalizerEntered := make(chan struct{})
		releaseFinalizer := make(chan struct{})
		released := false
		t.Cleanup(func() {
			if !released {
				close(releaseFinalizer)
			}
		})
		if err := fixture.runtime.AddFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.AddFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}

		received := make(chan PeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "receive before blocked finalization", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})
		fixture.runtime.RecordError(errors.New("runtime failed before finalizers completed"))
		fixture.runtime.Fail(context.Canceled)
		finished := make(chan struct{})
		go func() {
			fixture.runtime.Close()
			close(finished)
		}()
		waitForReceiverPeerSignal(t, finalizerEntered, "runtime finalizer gap")
		assertReceiverRuntimeDoneOpen(t, fixture.runtime)

		waitReceiverPeerState(t, fixture.operation, "runtime-owned receive terminal", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return transition.authority == PeerTerminalAuthorityRuntime && !receiving
		})
		assertReceiverPeerRuntimeTermination(t, fixture.operation, requireReceiverPeerTermination(t, <-received))

		close(releaseFinalizer)
		released = true
		waitForReceiverPeerSignal(t, finished, "runtime Done publication")
	})

	t.Run("receive retains simultaneous caller cancellation diagnostic", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb9)
		finalizerEntered := make(chan struct{})
		releaseFinalizer := make(chan struct{})
		released := false
		t.Cleanup(func() {
			if !released {
				close(releaseFinalizer)
			}
		})
		if err := fixture.runtime.AddFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.AddFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}

		caller, cancelCaller := context.WithCancel(context.Background())
		t.Cleanup(cancelCaller)
		received := make(chan PeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(caller) }()
		waitReceiverPeerState(t, fixture.operation, "receive before simultaneous cancellation", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ PeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})
		fixture.runtime.RecordError(errors.New("runtime failed while caller canceled"))
		fixture.runtime.Fail(context.Canceled)
		finished := make(chan struct{})
		go func() {
			fixture.runtime.Close()
			close(finished)
		}()
		waitForReceiverPeerSignal(t, finalizerEntered, "runtime lifecycle cancellation")
		assertReceiverRuntimeDoneOpen(t, fixture.runtime)
		cancelCaller()

		termination := requireReceiverPeerTermination(t, <-received)
		assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)
		if !receiverPeerDiagnosticsContain(termination.Diagnostics(), PeerDiagnosticContextCanceled) {
			t.Fatalf("simultaneous terminal dropped caller cancellation diagnostic: %+v", termination.Diagnostics().Components())
		}

		close(releaseFinalizer)
		released = true
		waitForReceiverPeerSignal(t, finished, "runtime Done publication")
	})

	t.Run("terminate observes lifecycle before finalizers close call", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb8)
		finalizerEntered := make(chan struct{})
		releaseFinalizer := make(chan struct{})
		released := false
		t.Cleanup(func() {
			if !released {
				close(releaseFinalizer)
			}
		})
		if err := fixture.runtime.AddFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.AddFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}

		fixture.runtime.RecordError(errors.New("runtime failed before terminate"))
		fixture.runtime.Fail(context.Canceled)
		finished := make(chan struct{})
		go func() {
			fixture.runtime.Close()
			close(finished)
		}()
		waitForReceiverPeerSignal(t, finalizerEntered, "runtime lifecycle gap")
		assertReceiverRuntimeDoneOpen(t, fixture.runtime)

		termination := fixture.operation.Terminate(context.Background())
		assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)

		close(releaseFinalizer)
		released = true
		waitForReceiverPeerSignal(t, finished, "runtime Done publication")
	})
}

func TestReceiverPeerInvalidValuesFailClosed(t *testing.T) {
	// These calls intentionally exercise the public fail-closed contract without
	// manufacturing context authority that an invalid receiver cannot own.
	var missingContext context.Context
	if _, err := (&Runtime{}).OpenPeerOperation(missingContext, nil); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("invalid receiver runtime open error = %v", err)
	}
	for name, operation := range map[string]*PeerOperation{
		"nil operation":  nil,
		"zero operation": {},
		"missing rpc with call": {
			call: &sessioncore.Call{},
		},
		"missing runtime": {
			rpc:  &sessioncore.Requests{},
			call: &sessioncore.Call{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := operation.MaximumContinuations(); ok {
				t.Fatal("invalid peer operation exposed a continuation budget")
			}
			if _, err := operation.SendCandidate(missingContext, nil); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
				t.Fatalf("invalid peer candidate error = %v", err)
			}
			if control, ok := operation.Receive(missingContext).Control(); ok {
				t.Fatalf("invalid peer operation exposed control: %+v", control)
			}
			if termination, ok := operation.Receive(missingContext).Termination(); ok {
				t.Fatalf("invalid peer operation manufactured a termination: %+v", termination)
			}
			termination := operation.Terminate(missingContext)
			if operation != nil && operation.OwnsTermination(termination) {
				t.Fatalf("invalid peer operation validated a terminal value: %+v", termination)
			}
		})
	}
}

type receiverPeerTerminalFixture struct {
	runtime   *sessioncore.Runtime
	operation *PeerOperation
	call      *sessioncore.Call
}

func newReceiverPeerTerminalFixture(t *testing.T, seed byte) receiverPeerTerminalFixture {
	t.Helper()
	runtime, _ := newUnstartedRuntimeWithContinuations(
		t,
		protocolsession.RoleReceiver,
		protocolsession.OperationLimits{},
		nil,
		continuationReplayClassifier{},
	)
	return openReceiverPeerTerminalFixture(t, runtime, seed)
}

func openReceiverPeerTerminalFixture(t *testing.T, runtime *sessioncore.Runtime, seed byte) receiverPeerTerminalFixture {
	t.Helper()
	startReceiverPeerWriter(t, runtime)

	rpc := newTestRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{seed}, protocolsession.IdentityBytes)))
	if err := rpc.Register(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpc.Close)
	receiver := &Runtime{core: runtime, lifecycle: runtime, rpc: rpc}
	operation, err := receiver.OpenPeerOperation(context.Background(), []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	operation.mu.Lock()
	call := operation.call
	operation.mu.Unlock()
	if call == nil {
		t.Fatal("opened peer operation did not retain its exact call")
	}
	t.Cleanup(func() { _ = operation.Terminate(context.Background()) })
	return receiverPeerTerminalFixture{runtime: runtime, operation: operation, call: call}
}

func enqueueUnexpectedPeerResponse(t *testing.T, call *sessioncore.Call) {
	t.Helper()
	message, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult,
		new(call.ID()),
		[]byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	enqueueCallResponse(call, message)
}

func waitReceiverPeerState(
	t *testing.T,
	operation *PeerOperation,
	description string,
	condition func(
		receiverPeerTerminalTransition,
		receiverPeerTerminalConsequence,
		PeerDiagnosticSnapshot,
		bool,
	) bool,
) {
	t.Helper()
	waitSessionCondition(t, description, func() bool {
		operation.mu.Lock()
		defer operation.mu.Unlock()
		return condition(
			operation.terminalTransition,
			operation.terminalConsequence,
			operation.terminalDiagnostics,
			operation.receiving,
		)
	})
}

func requireReceiverPeerTermination(
	t *testing.T,
	result PeerReceiveResult,
) PeerTermination {
	t.Helper()
	termination, ok := result.Termination()
	if !ok {
		control, controlOK := result.Control()
		t.Fatalf("peer receive did not return a termination: control=%+v present=%t", control, controlOK)
	}
	if control, ok := result.Control(); ok {
		t.Fatalf("terminal peer result also exposed control: %+v", control)
	}
	return termination
}

func requireReceiverPeerControl(
	t *testing.T,
	result PeerReceiveResult,
) PeerControl {
	t.Helper()
	control, ok := result.Control()
	if !ok {
		termination, terminalOK := result.Termination()
		t.Fatalf("peer receive did not return control: termination=%+v present=%t", termination, terminalOK)
	}
	if termination, ok := result.Termination(); ok {
		t.Fatalf("control peer result also exposed termination: %+v", termination)
	}
	return control
}

func assertReceiverPeerTermination(
	t *testing.T,
	operation *PeerOperation,
	termination PeerTermination,
	wantAuthority PeerTerminalAuthority,
	wantTransition PeerTerminalProvenance,
	wantSeverity PeerTerminalSeverity,
	wantConsequence PeerTerminalProvenance,
	wantDiagnostics ...PeerDiagnosticCode,
) {
	t.Helper()
	if operation == nil || !operation.OwnsTermination(termination) {
		t.Fatalf("peer operation rejected its published termination: %+v", termination)
	}
	if termination.Authority() != wantAuthority ||
		termination.TransitionProvenance() != wantTransition ||
		termination.Severity() != wantSeverity ||
		termination.ConsequenceProvenance() != wantConsequence {
		t.Fatalf(
			"peer termination authority=%d transition=%d severity=%d consequence=%d, want %d/%d/%d/%d",
			termination.Authority(),
			termination.TransitionProvenance(),
			termination.Severity(),
			termination.ConsequenceProvenance(),
			wantAuthority,
			wantTransition,
			wantSeverity,
			wantConsequence,
		)
	}
	for _, code := range wantDiagnostics {
		if !receiverPeerDiagnosticsContain(termination.Diagnostics(), code) {
			t.Fatalf("peer termination diagnostics=%+v, want code=%d", termination.Diagnostics().Components(), code)
		}
	}
}

func assertReceiverPeerRuntimeTermination(
	t *testing.T,
	operation *PeerOperation,
	termination PeerTermination,
) {
	t.Helper()
	assertReceiverPeerTermination(
		t,
		operation,
		termination,
		PeerTerminalAuthorityRuntime,
		PeerProvenanceRuntimeStopping,
		PeerTerminalSessionUnavailable,
		PeerProvenanceRuntimeStopping,
		PeerDiagnosticRuntimeClosed,
	)
}

func receiverPeerDiagnosticsContain(
	diagnostics PeerDiagnosticSnapshot,
	want PeerDiagnosticCode,
) bool {
	for _, diagnostic := range diagnostics.Components() {
		if diagnostic.Code() == want {
			return true
		}
	}
	return false
}

func requireReceiverPeerRemoteFailure(
	t *testing.T,
	termination PeerTermination,
	wantCode PeerDiagnosticCode,
) RemoteOperationFailureSnapshot {
	t.Helper()
	for _, diagnostic := range termination.Diagnostics().Components() {
		if diagnostic.Code() != wantCode {
			continue
		}
		failure, ok := diagnostic.RemoteFailure()
		if !ok {
			t.Fatalf("peer diagnostic code=%d omitted its remote failure snapshot", wantCode)
		}
		return failure
	}
	t.Fatalf("peer termination diagnostics=%+v, want remote code=%d", termination.Diagnostics().Components(), wantCode)
	return RemoteOperationFailureSnapshot{}
}

func assertReceiverPeerTerminationsEqual(
	t *testing.T,
	first PeerTermination,
	second PeerTermination,
) {
	t.Helper()
	if first.Authority() != second.Authority() ||
		first.TransitionProvenance() != second.TransitionProvenance() ||
		first.Severity() != second.Severity() ||
		first.ConsequenceProvenance() != second.ConsequenceProvenance() ||
		!equalReceiverPeerDiagnostics(first.Diagnostics(), second.Diagnostics()) {
		t.Fatalf("joined terminal outcome changed: first=%+v second=%+v", first, second)
	}
}

func equalReceiverPeerDiagnostics(
	left PeerDiagnosticSnapshot,
	right PeerDiagnosticSnapshot,
) bool {
	leftComponents := left.Components()
	rightComponents := right.Components()
	if left.Truncated() != right.Truncated() || len(leftComponents) != len(rightComponents) {
		return false
	}
	for index := range leftComponents {
		leftRemote, leftHasRemote := leftComponents[index].RemoteFailure()
		rightRemote, rightHasRemote := rightComponents[index].RemoteFailure()
		if leftComponents[index].Code() != rightComponents[index].Code() ||
			leftHasRemote != rightHasRemote || leftRemote != rightRemote {
			return false
		}
	}
	return true
}

func waitForReceiverPeerSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	waitSessionCondition(t, description, func() bool {
		select {
		case <-signal:
			return true
		default:
			return false
		}
	})
}

func assertReceiverRuntimeDoneOpen(t *testing.T, runtime *sessioncore.Runtime) {
	t.Helper()
	if runtime.Lifetime().Err() == nil {
		t.Fatal("runtime lifecycle remained active inside finalization")
	}
	select {
	case <-runtime.Done():
		t.Fatal("runtime Done published before blocked finalizer completed")
	default:
	}
}
