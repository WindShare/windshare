package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverPeerTerminalConsequenceDominanceIsIndependentOfTransitionOrder(t *testing.T) {
	local := newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalOperationContract,
		ReceiverPeerTerminalOperationOnly,
		receiverPeerDiagnostic(ReceiverPeerDiagnosticOperationOverflow),
	)
	unsafeRemote := newReceiverPeerTerminalEvidence(
		ReceiverPeerTerminalAuthorityRemote,
		ReceiverPeerProvenanceRemoteFailureScopeViolation,
		ReceiverPeerTerminalSessionUnsafe,
		receiverPeerDiagnostic(ReceiverPeerDiagnosticRemoteFailureScopeViolation),
	)

	for _, test := range []struct {
		name           string
		first          receiverPeerTerminalEvidence
		second         receiverPeerTerminalEvidence
		wantAuthority  ReceiverPeerTerminalAuthority
		wantTransition ReceiverPeerTerminalProvenance
	}{
		{
			name:           "local transition precedes unsafe remote consequence",
			first:          local,
			second:         unsafeRemote,
			wantAuthority:  ReceiverPeerTerminalAuthorityLocal,
			wantTransition: ReceiverPeerProvenanceLocalOperationContract,
		},
		{
			name:           "unsafe remote transition precedes local diagnostic",
			first:          unsafeRemote,
			second:         local,
			wantAuthority:  ReceiverPeerTerminalAuthorityRemote,
			wantTransition: ReceiverPeerProvenanceRemoteFailureScopeViolation,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := &ReceiverPeerOperation{
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
				ReceiverPeerTerminalSessionUnsafe,
				ReceiverPeerProvenanceRemoteFailureScopeViolation,
				ReceiverPeerDiagnosticOperationOverflow,
				ReceiverPeerDiagnosticRemoteFailureScopeViolation,
			)
		})
	}
}

func TestReceiverPeerTerminalRaceRetainsAuthenticatedProtocolDiagnostic(t *testing.T) {
	t.Run("local transition owns before remote failure", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb1)
		operationID := fixture.operation.OperationID()
		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "active receive", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})

		// Holding the lane lock leaves the authenticated response sink alive after
		// local transition ownership is committed, forcing the losing remote fact to join.
		fixture.call.laneMu.Lock()
		laneLocked := true
		t.Cleanup(func() {
			if laneLocked {
				fixture.call.laneMu.Unlock()
			}
		})
		terminated := make(chan ReceiverPeerTermination, 1)
		go func() { terminated <- fixture.operation.Terminate(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "local terminal ownership", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			_ bool,
		) bool {
			return transition.authority == ReceiverPeerTerminalAuthorityLocal
		})
		enqueueUnexpectedPeerResponse(t, fixture.call)
		waitReceiverPeerState(t, fixture.operation, "losing remote protocol diagnostic", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			diagnostics ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return transition.authority == ReceiverPeerTerminalAuthorityLocal && !receiving &&
				receiverPeerDiagnosticsContain(diagnostics, ReceiverPeerDiagnosticUnknownControl)
		})
		fixture.call.laneMu.Unlock()
		laneLocked = false

		receiveTermination := requireReceiverPeerTermination(t, <-received)
		termination := <-terminated
		for _, observed := range []ReceiverPeerTermination{receiveTermination, termination} {
			assertReceiverPeerTermination(
				t,
				fixture.operation,
				observed,
				ReceiverPeerTerminalAuthorityLocal,
				ReceiverPeerProvenanceLocalExplicitStop,
				ReceiverPeerTerminalOperationOnly,
				ReceiverPeerProvenanceLocalExplicitStop,
				ReceiverPeerDiagnosticUnknownControl,
			)
		}
		if fixture.operation.OperationID() != operationID {
			t.Fatal("local-owned terminal transition changed its stable operation identity")
		}
	})

	t.Run("remote failure owns before local join", func(t *testing.T) {
		fixture := newReceiverPeerTerminalFixture(t, 0xb2)
		operationID := fixture.operation.OperationID()
		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "active receive", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})

		fixture.call.laneMu.Lock()
		laneLocked := true
		t.Cleanup(func() {
			if laneLocked {
				fixture.call.laneMu.Unlock()
			}
		})
		enqueueUnexpectedPeerResponse(t, fixture.call)
		waitReceiverPeerState(t, fixture.operation, "remote terminal ownership", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			diagnostics ReceiverPeerDiagnosticSnapshot,
			_ bool,
		) bool {
			return transition.authority == ReceiverPeerTerminalAuthorityRemote &&
				receiverPeerDiagnosticsContain(diagnostics, ReceiverPeerDiagnosticUnknownControl)
		})
		terminated := make(chan ReceiverPeerTermination, 1)
		go func() { terminated <- fixture.operation.Terminate(context.Background()) }()
		select {
		case premature := <-terminated:
			t.Fatalf("local join returned before exact remote cleanup: %+v", premature)
		default:
		}
		fixture.call.laneMu.Unlock()
		laneLocked = false

		receiveTermination := requireReceiverPeerTermination(t, <-received)
		termination := <-terminated
		for _, observed := range []ReceiverPeerTermination{receiveTermination, termination} {
			assertReceiverPeerTermination(
				t,
				fixture.operation,
				observed,
				ReceiverPeerTerminalAuthorityRemote,
				ReceiverPeerProvenanceRemoteUnknownControl,
				ReceiverPeerTerminalOperationOnly,
				ReceiverPeerProvenanceRemoteUnknownControl,
				ReceiverPeerDiagnosticUnknownControl,
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
		ReceiverPeerTerminalAuthorityRemote,
		ReceiverPeerProvenanceRemoteUnknownControl,
		ReceiverPeerTerminalOperationOnly,
		receiverPeerDiagnostic(ReceiverPeerDiagnosticUnknownControl),
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
		ReceiverPeerTerminalAuthorityRemote,
		ReceiverPeerProvenanceRemoteUnknownControl,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceRemoteUnknownControl,
		ReceiverPeerDiagnosticUnknownControl,
	)
	foreign := &ReceiverPeerOperation{token: new(receiverPeerOperationToken)}
	if foreign.OwnsTermination(published) {
		t.Fatal("a different operation accepted the published terminal capability")
	}
	components := published.Diagnostics().Components()
	components[0] = ReceiverPeerDiagnostic{}
	if !receiverPeerDiagnosticsContain(published.Diagnostics(), ReceiverPeerDiagnosticUnknownControl) {
		t.Fatal("mutating a diagnostic slice changed the published terminal snapshot")
	}
	if receiverPeerDiagnosticsContain(late.Diagnostics(), ReceiverPeerDiagnosticRuntimeClosed) ||
		receiverPeerDiagnosticsContain(late.Diagnostics(), ReceiverPeerDiagnosticOpaqueFailure) {
		t.Fatalf("published terminal retained late claimant diagnostics: %+v", late.Diagnostics().Components())
	}
}

func TestReceiverPeerReceiveRetainsCancellationDiagnostics(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xb3)
	operationID := fixture.operation.OperationID()
	generation, _ := fixture.call.operationAuthority()
	fixture.runtime.lanes.mu.Lock()
	fixture.runtime.lanes.active[fixture.runtime.initial.ID].closing = true
	fixture.runtime.lanes.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	termination := requireReceiverPeerTermination(t, fixture.operation.Receive(ctx))
	assertReceiverPeerTermination(
		t,
		fixture.operation,
		termination,
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalContextEnded,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalContextEnded,
		ReceiverPeerDiagnosticContextCanceled,
		ReceiverPeerDiagnosticCleanupFailed,
	)
	if fixture.operation.OperationID() != operationID {
		t.Fatal("failed local cancellation changed its stable operation identity")
	}
	if generation.IsActive() || fixture.runtime.operations.ActiveCount() != 0 ||
		fixture.runtime.operations.TombstoneCount() != 1 {
		t.Fatalf(
			"failed local cancellation lifecycle active=%t operations=%d tombstones=%d",
			generation.IsActive(), fixture.runtime.operations.ActiveCount(),
			fixture.runtime.operations.TombstoneCount(),
		)
	}
	repeated := fixture.operation.Terminate(context.Background())
	assertReceiverPeerTerminationsEqual(t, termination, repeated)
}

func TestReceiverPeerRuntimeShutdownOwnsTerminalOutcome(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xb4)
	operationID := fixture.operation.OperationID()
	received := make(chan ReceiverPeerReceiveResult, 1)
	go func() { received <- fixture.operation.Receive(context.Background()) }()
	waitReceiverPeerState(t, fixture.operation, "receive before runtime shutdown", func(
		_ receiverPeerTerminalTransition,
		_ receiverPeerTerminalConsequence,
		_ ReceiverPeerDiagnosticSnapshot,
		receiving bool,
	) bool {
		return receiving
	})

	fixture.runtime.abortBeforeStart()
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
		if err := fixture.runtime.addFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.addFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}

		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(context.Background()) }()
		waitReceiverPeerState(t, fixture.operation, "receive before blocked finalization", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})
		fixture.runtime.recordError(errors.New("runtime failed before finalizers completed"))
		fixture.runtime.cancel()
		finished := make(chan struct{})
		go func() {
			fixture.runtime.finish()
			close(finished)
		}()
		waitForReceiverPeerSignal(t, finalizerEntered, "runtime finalizer gap")
		assertReceiverRuntimeDoneOpen(t, fixture.runtime)

		waitReceiverPeerState(t, fixture.operation, "runtime-owned receive terminal", func(
			transition receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return transition.authority == ReceiverPeerTerminalAuthorityRuntime && !receiving
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
		if err := fixture.runtime.addFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.addFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}

		caller, cancelCaller := context.WithCancel(context.Background())
		t.Cleanup(cancelCaller)
		received := make(chan ReceiverPeerReceiveResult, 1)
		go func() { received <- fixture.operation.Receive(caller) }()
		waitReceiverPeerState(t, fixture.operation, "receive before simultaneous cancellation", func(
			_ receiverPeerTerminalTransition,
			_ receiverPeerTerminalConsequence,
			_ ReceiverPeerDiagnosticSnapshot,
			receiving bool,
		) bool {
			return receiving
		})
		fixture.runtime.recordError(errors.New("runtime failed while caller canceled"))
		fixture.runtime.cancel()
		finished := make(chan struct{})
		go func() {
			fixture.runtime.finish()
			close(finished)
		}()
		waitForReceiverPeerSignal(t, finalizerEntered, "runtime lifecycle cancellation")
		assertReceiverRuntimeDoneOpen(t, fixture.runtime)
		cancelCaller()

		termination := requireReceiverPeerTermination(t, <-received)
		assertReceiverPeerRuntimeTermination(t, fixture.operation, termination)
		if !receiverPeerDiagnosticsContain(termination.Diagnostics(), ReceiverPeerDiagnosticContextCanceled) {
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
		if err := fixture.runtime.addFinalizer(func() {
			close(finalizerEntered)
			<-releaseFinalizer
		}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.runtime.addFinalizer(fixture.operation.rpc.Close); err != nil {
			t.Fatal(err)
		}

		fixture.runtime.recordError(errors.New("runtime failed before terminate"))
		fixture.runtime.cancel()
		finished := make(chan struct{})
		go func() {
			fixture.runtime.finish()
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
	if _, err := (&ReceiverRuntime{}).OpenPeerOperation(missingContext, nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("invalid receiver runtime open error = %v", err)
	}
	for name, operation := range map[string]*ReceiverPeerOperation{
		"nil operation":  nil,
		"zero operation": {},
		"missing rpc with call": {
			call: &operationCall{
				id:       id16[protocolsession.OperationID](0xb6),
				messages: make(chan operationResponse, 1),
			},
		},
		"missing runtime": {
			rpc: &rpcClient{},
			call: &operationCall{
				id:       id16[protocolsession.OperationID](0xb5),
				messages: make(chan operationResponse, 1),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := operation.MaximumContinuations(); ok {
				t.Fatal("invalid peer operation exposed a continuation budget")
			}
			if _, err := operation.SendCandidate(missingContext, nil); !errors.Is(err, ErrRuntimeClosed) {
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
	runtime   *runtimeCore
	operation *ReceiverPeerOperation
	call      *operationCall
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
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	t.Cleanup(func() {
		stopWriter()
		<-writerDone
	})

	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{seed}, protocolsession.IdentityBytes)))
	receiver := &ReceiverRuntime{runtimeCore: runtime, rpc: rpc}
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

func enqueueUnexpectedPeerResponse(t *testing.T, call *operationCall) {
	t.Helper()
	message, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult,
		&call.id,
		[]byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	enqueueCallResponse(call, message)
}

func waitReceiverPeerState(
	t *testing.T,
	operation *ReceiverPeerOperation,
	description string,
	condition func(
		receiverPeerTerminalTransition,
		receiverPeerTerminalConsequence,
		ReceiverPeerDiagnosticSnapshot,
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
	result ReceiverPeerReceiveResult,
) ReceiverPeerTermination {
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
	result ReceiverPeerReceiveResult,
) ReceiverPeerControl {
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
	operation *ReceiverPeerOperation,
	termination ReceiverPeerTermination,
	wantAuthority ReceiverPeerTerminalAuthority,
	wantTransition ReceiverPeerTerminalProvenance,
	wantSeverity ReceiverPeerTerminalSeverity,
	wantConsequence ReceiverPeerTerminalProvenance,
	wantDiagnostics ...ReceiverPeerDiagnosticCode,
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
	operation *ReceiverPeerOperation,
	termination ReceiverPeerTermination,
) {
	t.Helper()
	assertReceiverPeerTermination(
		t,
		operation,
		termination,
		ReceiverPeerTerminalAuthorityRuntime,
		ReceiverPeerProvenanceRuntimeStopping,
		ReceiverPeerTerminalSessionUnavailable,
		ReceiverPeerProvenanceRuntimeStopping,
		ReceiverPeerDiagnosticRuntimeClosed,
	)
}

func receiverPeerDiagnosticsContain(
	diagnostics ReceiverPeerDiagnosticSnapshot,
	want ReceiverPeerDiagnosticCode,
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
	termination ReceiverPeerTermination,
	wantCode ReceiverPeerDiagnosticCode,
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
	first ReceiverPeerTermination,
	second ReceiverPeerTermination,
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
	left ReceiverPeerDiagnosticSnapshot,
	right ReceiverPeerDiagnosticSnapshot,
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

func assertReceiverRuntimeDoneOpen(t *testing.T, runtime *runtimeCore) {
	t.Helper()
	if runtime.ctx.Err() == nil {
		t.Fatal("runtime lifecycle remained active inside finalization")
	}
	select {
	case <-runtime.Done():
		t.Fatal("runtime Done published before blocked finalizer completed")
	default:
	}
}
