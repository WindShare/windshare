package v2peer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type receiverScriptedTerminalOperation struct {
	id protocolsession.OperationID

	bindingMu sync.Mutex
	binding   ReceiverSignalingOperationBinding

	receiveStarted  chan struct{}
	releaseReceive  chan struct{}
	receiveOnce     sync.Once
	receiveTerminal func(ReceiverSignalingOperationBinding) ReceiverSignalingTermination

	terminateCalled   chan struct{}
	terminateOnce     sync.Once
	terminateTerminal func(ReceiverSignalingOperationBinding) ReceiverSignalingTermination
}

func newReceiverScriptedTerminalOperation() *receiverScriptedTerminalOperation {
	return &receiverScriptedTerminalOperation{
		id:              testOperationID(211),
		receiveStarted:  make(chan struct{}),
		terminateCalled: make(chan struct{}),
	}
}

func (operation *receiverScriptedTerminalOperation) bindReceiverSignalingOperation(
	binding ReceiverSignalingOperationBinding,
) {
	operation.bindingMu.Lock()
	operation.binding = binding
	operation.bindingMu.Unlock()
}

func (operation *receiverScriptedTerminalOperation) bindingSnapshot() ReceiverSignalingOperationBinding {
	operation.bindingMu.Lock()
	defer operation.bindingMu.Unlock()
	return operation.binding
}

func (operation *receiverScriptedTerminalOperation) OperationID() protocolsession.OperationID {
	return operation.id
}

func (*receiverScriptedTerminalOperation) MaximumContinuations() (int, bool) {
	return DefaultMaxCandidates, true
}

func (*receiverScriptedTerminalOperation) SendCandidate(
	context.Context,
	[]byte,
) (protocolsession.OperationDisposition, error) {
	return protocolsession.OperationDeliver, nil
}

func (operation *receiverScriptedTerminalOperation) Receive(
	ctx context.Context,
) ReceiverSignalingReceiveResult {
	operation.receiveOnce.Do(func() { close(operation.receiveStarted) })
	if operation.releaseReceive != nil {
		select {
		case <-ctx.Done():
		case <-operation.releaseReceive:
		}
	}
	binding := operation.bindingSnapshot()
	if operation.receiveTerminal == nil {
		return ReceiverSignalingReceiveResult{}
	}
	return NewReceiverSignalingTerminationResult(operation.receiveTerminal(binding))
}

func (operation *receiverScriptedTerminalOperation) Terminate(
	context.Context,
) ReceiverSignalingTermination {
	operation.terminateOnce.Do(func() { close(operation.terminateCalled) })
	if operation.terminateTerminal == nil {
		return ReceiverSignalingTermination{}
	}
	return operation.terminateTerminal(operation.bindingSnapshot())
}

func receiverBoundTestTermination(
	binding ReceiverSignalingOperationBinding,
	decision receiverAttemptDecision,
	cause error,
) ReceiverSignalingTermination {
	diagnostics, truncated := snapshotReceiverCauseWithStatus(cause)
	return ReceiverSignalingTermination{
		operationToken: binding.token,
		valid:          true,
		decision:       decision,
		diagnostics:    diagnostics, diagnosticsTruncated: truncated,
	}
}

func receiverUnsafeContinuationDecision() receiverAttemptDecision {
	return receiverAttemptDecision{
		transitionOwner:       ReceiverTerminalRemote,
		transitionProvenance:  ReceiverProvenanceAuthenticatedContinuationAuthority,
		disposition:           ReceiverDispositionSessionUnsafe,
		consequenceProvenance: ReceiverProvenanceAuthenticatedContinuationAuthority,
	}
}

func TestReceiverTerminationMergeIsMonotonicInBothOrders(t *testing.T) {
	weak := receiverOperationDecision(
		ReceiverTerminalRemote,
		ReceiverProvenanceRemoteOperationRejected,
	)
	unsafe := receiverUnsafeContinuationDecision()
	for _, test := range []struct {
		name              string
		receiveDecision   receiverAttemptDecision
		terminateDecision receiverAttemptDecision
		wantTransition    ReceiverTerminalProvenance
	}{
		{
			name:            "unsafe receive then weaker terminate",
			receiveDecision: unsafe, terminateDecision: weak,
			wantTransition: ReceiverProvenanceAuthenticatedContinuationAuthority,
		},
		{
			name:            "weaker receive then unsafe terminate",
			receiveDecision: weak, terminateDecision: unsafe,
			wantTransition: ReceiverProvenanceRemoteOperationRejected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := newReceiverScriptedTerminalOperation()
			operation.receiveTerminal = func(binding ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
				return receiverBoundTestTermination(binding, test.receiveDecision, nil)
			}
			operation.terminateTerminal = func(binding ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
				return receiverBoundTestTermination(binding, test.terminateDecision, nil)
			}
			harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
				signaling.operation = operation
			})
			receiveTest(t, harness.attempt.Done())

			outcome := harness.attempt.Outcome()
			if outcome.TransitionProvenance() != test.wantTransition ||
				outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
				outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedContinuationAuthority ||
				!outcome.RequiresSessionClose() {
				t.Fatalf("monotonic termination outcome=%+v", outcome)
			}
		})
	}
}

func TestReceiverTerminationPublicationWaitsForReceiveImport(t *testing.T) {
	operation := newReceiverScriptedTerminalOperation()
	operation.releaseReceive = make(chan struct{})
	operation.receiveTerminal = func(binding ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
		return receiverBoundTestTermination(binding, receiverUnsafeContinuationDecision(), nil)
	}
	operation.terminateTerminal = func(binding ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
		return NewReceiverSignalingLocalTermination(binding, nil)
	}
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.operation = operation
	})
	receiveTest(t, operation.receiveStarted)

	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()
	receiveTest(t, operation.terminateCalled)
	select {
	case err := <-closed:
		t.Fatalf("termination published before in-flight Receive import: %v", err)
	default:
	}
	close(operation.releaseReceive)
	if err := receiveTest(t, closed); err != nil {
		t.Fatalf("exact termination close: %v", err)
	}
	if outcome := harness.attempt.Outcome(); outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedContinuationAuthority {
		t.Fatalf("receive import lost unsafe consequence: %+v", outcome)
	}
}

func TestReceiverInvalidLateTerminationCannotDowngradeUnsafe(t *testing.T) {
	operation := newReceiverScriptedTerminalOperation()
	operation.receiveTerminal = func(binding ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
		return receiverBoundTestTermination(binding, receiverUnsafeContinuationDecision(), nil)
	}
	operation.terminateTerminal = func(ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
		return ReceiverSignalingTermination{}
	}
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.operation = operation
	})
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceAuthenticatedContinuationAuthority ||
		!errors.Is(outcome.RetainedCause(), ErrProtocol) {
		t.Fatalf("invalid late terminal downgraded outcome=%+v retained=%v", outcome, outcome.RetainedCause())
	}
}

func TestReceiverTerminationRejectsCrossOperationReplayWithSameWireID(t *testing.T) {
	firstBinding := newReceiverSignalingOperationBinding()
	secondBinding := newReceiverSignalingOperationBinding()
	stale := receiverBoundTestTermination(
		firstBinding,
		receiverUnsafeContinuationDecision(),
		nil,
	)
	operation := newReceiverScriptedTerminalOperation()
	operation.terminateTerminal = func(ReceiverSignalingOperationBinding) ReceiverSignalingTermination {
		return stale
	}
	operation.bindReceiverSignalingOperation(secondBinding)
	bound := newReceiverBoundSignalingOperation(operation, operation.id, secondBinding)

	termination := bound.terminateExact()
	if firstBinding.localGeneration == secondBinding.localGeneration ||
		!termination.ownedBy(secondBinding) ||
		termination.decision.disposition != ReceiverDispositionFallbackAllowed ||
		termination.decision.transitionProvenance != ReceiverProvenanceSignalingAdapterContract ||
		termination.decision.consequenceProvenance != ReceiverProvenanceSignalingAdapterContract ||
		!errors.Is(termination.diagnostics, ErrProtocol) {
		t.Fatalf("cross-operation replay accepted: %+v", termination)
	}

	bound.terminationMu.Lock()
	bound.mergeTerminationLocked(receiverBoundTestTermination(
		secondBinding,
		receiverUnsafeContinuationDecision(),
		nil,
	))
	bound.terminationMu.Unlock()
	sealed := bound.terminationResult()
	if sealed.decision.disposition != ReceiverDispositionFallbackAllowed ||
		sealed.decision.transitionProvenance != ReceiverProvenanceSignalingAdapterContract {
		t.Fatalf("published terminal mutated after sealing: %+v", sealed)
	}
}

type receiverManualTimer struct {
	ticks   chan time.Time
	stopped atomic.Bool
}

func newReceiverManualTimer() *receiverManualTimer {
	return &receiverManualTimer{ticks: make(chan time.Time, 1)}
}

func (timer *receiverManualTimer) C() <-chan time.Time { return timer.ticks }
func (timer *receiverManualTimer) Stop()               { timer.stopped.Store(true) }

func (timer *receiverManualTimer) Fire() {
	if timer.stopped.Load() {
		return
	}
	timer.ticks <- time.Unix(1, 0)
}

type receiverManualTimerSource struct{ timer *receiverManualTimer }

func (source receiverManualTimerSource) NewReceiverAttemptTimer(
	time.Duration,
) (ReceiverAttemptTimer, error) {
	return source.timer, nil
}

type receiverCreateOfferFailurePeer struct {
	*receiverTestPeerConnection
	cause error
}

func (peer *receiverCreateOfferFailurePeer) CreateOffer(
	*pion.OfferOptions,
) (pion.SessionDescription, error) {
	return pion.SessionDescription{}, peer.cause
}

func startReceiverPreOperationAttempt(
	t *testing.T,
	parent context.Context,
	signaling ReceiverSignaling,
	peer ReceiverPeerConnection,
	timers ReceiverAttemptTimerSource,
) *ReceiverAttempt {
	t.Helper()
	channel := newReceiverTestChannel()
	config := ReceiverFactoryConfig{
		PeerConnections: ReceiverPeerConnectionFactoryFunc(
			func(pion.Configuration) (ReceiverPeerConnection, error) { return peer, nil },
		),
		DataChannels: DataChannelAdapterFunc(
			func(*pion.DataChannel) (PeerDataChannel, error) { return channel, nil },
		),
		AttemptTimeout: peerTestTimeout,
		AttemptTimers:  timers,
		Random:         bytes.NewReader(bytes.Repeat([]byte{0x6b}, v2signal.IdentityBytes*2)),
	}
	factory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := factory.Start(parent, signaling, newReceiverTestLanes())
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func assertReceiverPreOperationOutcome(
	t *testing.T,
	outcome ReceiverAttemptOutcome,
	owner ReceiverTerminalOwner,
	provenance ReceiverTerminalProvenance,
	disposition ReceiverAttemptDisposition,
) {
	t.Helper()
	if !outcome.OperationID().IsZero() || outcome.LocalGeneration() == 0 ||
		outcome.TransitionAuthority() != owner ||
		outcome.TransitionProvenance() != provenance ||
		outcome.Disposition() != disposition ||
		outcome.ConsequenceProvenance() != provenance {
		t.Fatalf("pre-operation outcome=%+v", outcome)
	}
}

func TestReceiverPreOperationCreateOfferFailureIsNegotiationLocal(t *testing.T) {
	cause := errors.New("create offer failed")
	base := newReceiverTestPeerConnection()
	signaling := &receiverTestSignaling{
		operation: newReceiverTestOperation(),
		offers:    make(chan []byte, 1),
	}
	attempt := startReceiverPreOperationAttempt(
		t,
		context.Background(),
		signaling,
		&receiverCreateOfferFailurePeer{receiverTestPeerConnection: base, cause: cause},
		nil,
	)
	receiveTest(t, attempt.Done())

	outcome := attempt.Outcome()
	assertReceiverPreOperationOutcome(
		t, outcome,
		ReceiverTerminalLocal,
		ReceiverProvenanceLocalNegotiationFailure,
		ReceiverDispositionFallbackAllowed,
	)
	if !errors.Is(outcome.RetainedCause(), ErrNegotiation) ||
		!errors.Is(outcome.RetainedCause(), cause) ||
		errors.Is(outcome.RetainedCause(), ErrProtocol) ||
		outcome.HasRetainedCauseClass(ReceiverCauseProtocol) {
		t.Fatalf("create-offer diagnostics=%v classes=%v", outcome.RetainedCause(), outcome.RetainedCauseClasses())
	}
}

func TestReceiverPreOperationOpenFailureIsNegotiationLocal(t *testing.T) {
	cause := errors.New("signaling open failed")
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.open = func(
			context.Context,
			ReceiverSignalingOperationBinding,
			[]byte,
		) (ReceiverSignalingOperation, error) {
			return nil, cause
		}
	})
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	assertReceiverPreOperationOutcome(
		t, outcome,
		ReceiverTerminalLocal,
		ReceiverProvenanceLocalNegotiationFailure,
		ReceiverDispositionFallbackAllowed,
	)
	if !errors.Is(outcome.RetainedCause(), ErrNegotiation) ||
		!errors.Is(outcome.RetainedCause(), cause) ||
		errors.Is(outcome.RetainedCause(), ErrProtocol) {
		t.Fatalf("open diagnostics=%v", outcome.RetainedCause())
	}
}

func TestReceiverPreOperationContextEndingsRemainLocal(t *testing.T) {
	for _, test := range []struct {
		name             string
		cause            error
		wantRetained     error
		wantBenignCancel bool
	}{
		{name: "cancel", cause: context.Canceled, wantBenignCancel: true},
		{
			name: "deadline", cause: context.DeadlineExceeded,
			wantRetained: context.DeadlineExceeded, wantBenignCancel: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent, cancel := context.WithCancelCause(context.Background())
			entered := make(chan struct{})
			harness := newReceiverHarnessWithContext(t, parent, func(
				_ *ReceiverFactoryConfig,
				signaling *receiverTestSignaling,
			) {
				signaling.open = func(
					ctx context.Context,
					_ ReceiverSignalingOperationBinding,
					_ []byte,
				) (ReceiverSignalingOperation, error) {
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			})
			receiveTest(t, entered)
			cancel(test.cause)
			receiveTest(t, harness.attempt.Done())

			outcome := harness.attempt.Outcome()
			assertReceiverPreOperationOutcome(
				t, outcome,
				ReceiverTerminalLocal,
				ReceiverProvenanceLocalContextEnded,
				ReceiverDispositionFallbackAllowed,
			)
			if !outcome.LocallyCanceled() || errors.Is(outcome.RetainedCause(), ErrNegotiation) ||
				errors.Is(outcome.RetainedCause(), ErrProtocol) {
				t.Fatalf("context ending diagnostics=%v outcome=%+v", outcome.RetainedCause(), outcome)
			}
			if test.wantRetained != nil && !errors.Is(outcome.RetainedCause(), test.wantRetained) {
				t.Fatalf("context ending retained=%v, want %v", outcome.RetainedCause(), test.wantRetained)
			}
			if test.wantRetained == nil && outcome.RetainedCause() != nil {
				t.Fatalf("clean cancellation retained=%v", outcome.RetainedCause())
			}
			if containsReceiverBenignCause(
				outcome.BenignComponents(),
				ReceiverBenignContextCanceled,
			) != test.wantBenignCancel {
				t.Fatalf("context ending benign=%v", outcome.BenignComponents())
			}
		})
	}
}

func TestReceiverPreOperationAttemptTimeoutUsesTypedLocalDecision(t *testing.T) {
	timer := newReceiverManualTimer()
	entered := make(chan struct{})
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, configured *receiverTestSignaling) {
		config.AttemptTimers = receiverManualTimerSource{timer: timer}
		configured.open = func(
			ctx context.Context,
			_ ReceiverSignalingOperationBinding,
			_ []byte,
		) (ReceiverSignalingOperation, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	receiveTest(t, entered)
	timer.Fire()
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	assertReceiverPreOperationOutcome(
		t, outcome,
		ReceiverTerminalLocal,
		ReceiverProvenanceLocalAttemptTimeout,
		ReceiverDispositionFallbackAllowed,
	)
	if !errors.Is(outcome.RetainedCause(), errAttemptTimeout) ||
		!outcome.HasRetainedCauseClass(ReceiverCauseAttemptTimeout) ||
		errors.Is(outcome.RetainedCause(), ErrProtocol) ||
		outcome.HasRetainedCauseClass(ReceiverCauseProtocol) {
		t.Fatalf("timeout diagnostics=%v classes=%v", outcome.RetainedCause(), outcome.RetainedCauseClasses())
	}
}

func TestReceiverPreOperationRuntimeStoppingIsUnavailable(t *testing.T) {
	runtimeSignaling, err := NewRuntimeReceiverSignaling(&sessionruntime.ReceiverRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	attempt := startReceiverPreOperationAttempt(
		t,
		parent,
		runtimeSignaling,
		newReceiverTestPeerConnection(),
		nil,
	)
	receiveTest(t, attempt.Done())

	outcome := attempt.Outcome()
	assertReceiverPreOperationOutcome(
		t, outcome,
		ReceiverTerminalRuntime,
		ReceiverProvenanceRuntimeStopping,
		ReceiverDispositionSessionUnavailable,
	)
	if !errors.Is(outcome.RetainedCause(), sessionruntime.ErrRuntimeClosed) ||
		errors.Is(outcome.RetainedCause(), ErrProtocol) || outcome.RequiresSessionClose() {
		t.Fatalf("runtime-stopping diagnostics=%v outcome=%+v", outcome.RetainedCause(), outcome)
	}
}

func TestReceiverPreOperationInvalidAdapterShapesUseContractProvenance(t *testing.T) {
	for _, test := range []struct {
		name string
		open func(*receiverTestOperation) func(
			context.Context,
			ReceiverSignalingOperationBinding,
			[]byte,
		) (ReceiverSignalingOperation, error)
		wantTerminate bool
	}{
		{
			name: "nil operation and nil error",
			open: func(*receiverTestOperation) func(
				context.Context,
				ReceiverSignalingOperationBinding,
				[]byte,
			) (ReceiverSignalingOperation, error) {
				return func(context.Context, ReceiverSignalingOperationBinding, []byte) (
					ReceiverSignalingOperation,
					error,
				) {
					return nil, nil
				}
			},
		},
		{
			name: "operation and error",
			open: func(operation *receiverTestOperation) func(
				context.Context,
				ReceiverSignalingOperationBinding,
				[]byte,
			) (ReceiverSignalingOperation, error) {
				return func(context.Context, ReceiverSignalingOperationBinding, []byte) (
					ReceiverSignalingOperation,
					error,
				) {
					return operation, errors.New("adapter returned an impossible result tuple")
				}
			},
			wantTerminate: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var operation *receiverTestOperation
			harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
				operation = signaling.operation.(*receiverTestOperation)
				signaling.open = test.open(operation)
			})
			receiveTest(t, harness.attempt.Done())

			outcome := harness.attempt.Outcome()
			if test.wantTerminate {
				if outcome.OperationID().IsZero() || outcome.LocalGeneration() == 0 ||
					outcome.TransitionAuthority() != ReceiverTerminalLocal ||
					outcome.TransitionProvenance() != ReceiverProvenanceSignalingAdapterContract ||
					outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
					outcome.ConsequenceProvenance() != ReceiverProvenanceSignalingAdapterContract {
					t.Fatalf("bound invalid-adapter outcome=%+v", outcome)
				}
			} else {
				assertReceiverPreOperationOutcome(
					t, outcome,
					ReceiverTerminalLocal,
					ReceiverProvenanceSignalingAdapterContract,
					ReceiverDispositionFallbackAllowed,
				)
			}
			if !errors.Is(outcome.RetainedCause(), ErrProtocol) ||
				operation.cancelCalls.Load() != map[bool]int32{false: 0, true: 1}[test.wantTerminate] {
				t.Fatalf("invalid adapter outcome=%+v calls=%d", outcome, operation.cancelCalls.Load())
			}
		})
	}
}

func TestReceiverCoreProvenanceMappingIsOneToOne(t *testing.T) {
	for core, want := range map[sessionruntime.ReceiverPeerTerminalProvenance]ReceiverTerminalProvenance{
		sessionruntime.ReceiverPeerProvenanceLocalExplicitStop:                    ReceiverProvenanceLocalExplicitStop,
		sessionruntime.ReceiverPeerProvenanceLocalContextEnded:                    ReceiverProvenanceLocalContextEnded,
		sessionruntime.ReceiverPeerProvenanceLocalOperationContract:               ReceiverProvenanceLocalOperationContract,
		sessionruntime.ReceiverPeerProvenanceRemoteOperationRejected:              ReceiverProvenanceRemoteOperationRejected,
		sessionruntime.ReceiverPeerProvenanceRemoteUnknownControl:                 ReceiverProvenanceRemoteUnknownControl,
		sessionruntime.ReceiverPeerProvenanceRemoteControlMalformed:               ReceiverProvenanceRemoteControlMalformed,
		sessionruntime.ReceiverPeerProvenanceRemoteFailureMalformed:               ReceiverProvenanceRemoteFailureMalformed,
		sessionruntime.ReceiverPeerProvenanceRemoteFailureScopeViolation:          ReceiverProvenanceRemoteFailureScopeViolation,
		sessionruntime.ReceiverPeerProvenanceRemoteAnswerConflict:                 ReceiverProvenanceAuthenticatedSecondAnswer,
		sessionruntime.ReceiverPeerProvenanceRemoteFinalConflict:                  ReceiverProvenanceAuthenticatedFinalConflict,
		sessionruntime.ReceiverPeerProvenanceRemoteContinuationAuthorityViolation: ReceiverProvenanceAuthenticatedContinuationAuthority,
		sessionruntime.ReceiverPeerProvenanceRuntimeStopping:                      ReceiverProvenanceRuntimeStopping,
	} {
		got, ok := receiverProvenanceFromCore(core)
		if !ok || got != want {
			t.Fatalf("core provenance %v mapped to %v, %t; want %v", core, got, ok, want)
		}
	}
	if ReceiverProvenanceAuthenticatedContinuationAuthority ==
		ReceiverProvenanceAuthenticatedCandidateBindingMismatch {
		t.Fatal("continuation authority collapsed into candidate binding provenance")
	}
}
