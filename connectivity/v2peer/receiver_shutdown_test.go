package v2peer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const concurrentReceiverCloses = 32

type exactReceiverTestOperation struct {
	*receiverTestOperation
	receiveEntered    chan struct{}
	remoteError       chan error
	remoteTerminal    chan struct{}
	releaseRemote     chan struct{}
	terminateWake     chan struct{}
	receiveReturned   chan struct{}
	gateRemote        bool
	remoteDecision    receiverAttemptDecision
	terminateDecision receiverAttemptDecision
	receiveOnce       sync.Once
	receiveReturnOnce sync.Once
	remoteOnce        sync.Once
	releaseOnce       sync.Once
	terminateOnce     sync.Once
	terminateCalls    atomic.Int32
	terminateCause    error
}

type receiverTeardownGates struct {
	channelCloseEntered chan struct{}
	breakFormerCycle    chan struct{}
	releaseChannelDrain chan struct{}
	breakOnce           sync.Once
	releaseOnce         sync.Once
}

func newReceiverTeardownGates() *receiverTeardownGates {
	return &receiverTeardownGates{
		channelCloseEntered: make(chan struct{}),
		breakFormerCycle:    make(chan struct{}),
		releaseChannelDrain: make(chan struct{}),
	}
}

func (gates *receiverTeardownGates) breakCycle() {
	gates.breakOnce.Do(func() { close(gates.breakFormerCycle) })
}

func (gates *receiverTeardownGates) releaseDrain() {
	gates.releaseOnce.Do(func() { close(gates.releaseChannelDrain) })
}

type receiverTeardownGateChannel struct {
	*receiverTestChannel
	peerShutdown <-chan struct{}
	gates        *receiverTeardownGates
	closeOnce    sync.Once
	closeDone    chan struct{}
	closeErr     error
}

func newReceiverTeardownGateChannel(
	channel *receiverTestChannel,
	peerShutdown <-chan struct{},
	gates *receiverTeardownGates,
) *receiverTeardownGateChannel {
	return &receiverTeardownGateChannel{
		receiverTestChannel: channel,
		peerShutdown:        peerShutdown,
		gates:               gates,
		closeDone:           make(chan struct{}),
	}
}

func (channel *receiverTeardownGateChannel) Close() error {
	channel.closeOnce.Do(func() {
		close(channel.gates.channelCloseEntered)
		select {
		case <-channel.peerShutdown:
		case <-channel.gates.breakFormerCycle:
		}
		<-channel.gates.releaseChannelDrain
		channel.closeErr = channel.receiverTestChannel.Close()
		close(channel.closeDone)
	})
	<-channel.closeDone
	return channel.closeErr
}

type receiverTeardownGateHarness struct {
	*receiverHarness
	operation *exactReceiverTestOperation
	peer      *receiverTestPeerConnection
	gates     *receiverTeardownGates
	traces    <-chan ReceiverTerminationTrace
}

func newReceiverTeardownGateHarness(t *testing.T) *receiverTeardownGateHarness {
	t.Helper()
	peer := newReceiverTestPeerConnection()
	baseChannel := newReceiverTestChannel()
	gates := newReceiverTeardownGates()
	channel := newReceiverTeardownGateChannel(baseChannel, peer.closed, gates)
	var operation *exactReceiverTestOperation
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		operation = newExactReceiverTestOperation(
			signaling.operation.(*receiverTestOperation),
			false,
		)
		operation.receiverTestOperation.channel = baseChannel
		signaling.operation = operation
		config.PeerConnections = ReceiverPeerConnectionFactoryFunc(
			func(pion.Configuration) (ReceiverPeerConnection, error) { return peer, nil },
		)
		config.DataChannels = DataChannelAdapterFunc(
			func(*pion.DataChannel) (PeerDataChannel, error) { return channel, nil },
		)
		config.ReceiverTerminationObservationCapacity = 1
	})
	traces := harness.factory.ReceiverTerminationObservations()
	t.Cleanup(func() {
		gates.breakCycle()
		gates.releaseDrain()
		operation.releaseRemoteError()
		_ = harness.attempt.Close()
	})
	receiveTest(t, operation.receiveEntered)
	return &receiverTeardownGateHarness{
		receiverHarness: harness,
		operation:       operation,
		peer:            peer,
		gates:           gates,
		traces:          traces,
	}
}

var expectedPeerTeardownTransitions = []PeerTeardownTransition{
	PeerTeardownPeerShutdownInitiated,
	PeerTeardownPeerShutdownReturned,
	PeerTeardownChannelDrainStarted,
	PeerTeardownChannelDrainJoined,
}

func assertReceiverTeardownReachedDrainAfterPeerShutdown(
	t *testing.T,
	harness *receiverTeardownGateHarness,
) {
	t.Helper()
	receiveTest(t, harness.gates.channelCloseEntered)
	select {
	case <-harness.peer.closed:
	default:
		t.Error("channel drain began before peer shutdown could break its terminal wait")
		// This branch releases the former implementation so a failing regression
		// remains leak-free and can publish the rest of its diagnostics.
		harness.gates.breakCycle()
	}
	select {
	case <-harness.attempt.Done():
		t.Error("receiver completion published before channel drain joined")
	default:
	}
	select {
	case trace := <-harness.traces:
		t.Errorf("termination trace published before channel drain joined: %+v", trace)
	default:
	}
}

func assertReceiverTeardownTrace(t *testing.T, trace ReceiverTerminationTrace) {
	t.Helper()
	if transitions := trace.TeardownTransitions(); !reflect.DeepEqual(
		transitions,
		expectedPeerTeardownTransitions,
	) {
		t.Fatalf("teardown transitions=%v, want %v", transitions, expectedPeerTeardownTransitions)
	}
	if trace.PeerShutdownFailed() || trace.ChannelDrainFailed() {
		t.Fatalf("clean teardown reported failure: %+v", trace)
	}
}

func newExactReceiverTestOperation(
	operation *receiverTestOperation,
	gateRemote bool,
) *exactReceiverTestOperation {
	return &exactReceiverTestOperation{
		receiverTestOperation: operation,
		receiveEntered:        make(chan struct{}),
		remoteError:           make(chan error, 1),
		remoteTerminal:        make(chan struct{}),
		releaseRemote:         make(chan struct{}),
		terminateWake:         make(chan struct{}),
		receiveReturned:       make(chan struct{}),
		gateRemote:            gateRemote,
		remoteDecision: receiverOperationDecision(
			ReceiverTerminalRemote,
			ReceiverProvenanceRemoteOperationRejected,
		),
		terminateDecision: receiverOperationDecision(
			ReceiverTerminalLocal,
			ReceiverProvenanceLocalExplicitStop,
		),
	}
}

func (operation *exactReceiverTestOperation) OperationID() protocolsession.OperationID {
	return operation.receiverTestOperation.OperationID()
}

func (operation *exactReceiverTestOperation) Receive(ctx context.Context) ReceiverSignalingReceiveResult {
	operation.receiveOnce.Do(func() { close(operation.receiveEntered) })
	select {
	case <-ctx.Done():
		termination := operation.finishTermination(
			ReceiverTerminalLocal,
			ctx.Err(),
		)
		operation.markReceiveReturned()
		return receiverTestTerminalResult(termination)
	case <-operation.terminateWake:
		operation.markReceiveReturned()
		return receiverTestTerminalResult(operation.terminationResult())
	case err := <-operation.remoteError:
		operation.finishTerminationWithDecision(operation.remoteDecision, err)
		operation.remoteOnce.Do(func() { close(operation.remoteTerminal) })
		if operation.gateRemote {
			<-operation.releaseRemote
		}
		operation.markReceiveReturned()
		return receiverTestTerminalResult(operation.terminationResult())
	case control := <-operation.controls:
		return NewReceiverSignalingControlResult(control)
	}
}

func (operation *exactReceiverTestOperation) Terminate(
	context.Context,
) ReceiverSignalingTermination {
	operation.terminateCalls.Add(1)
	operation.terminateOnce.Do(func() {
		operation.finishTerminationWithDecision(
			operation.terminateDecision,
			operation.terminateCause,
		)
		close(operation.terminateWake)
	})
	// Termination joins an active Receive, but an early Close may win before the
	// worker enters Receive. The bound operation prevents a new Receive after
	// that decision, so waiting in the latter case would deadlock the test double.
	select {
	case <-operation.receiveEntered:
		<-operation.receiveReturned
	default:
	}
	return operation.terminationResult()
}

func (operation *exactReceiverTestOperation) markReceiveReturned() {
	operation.receiveReturnOnce.Do(func() { close(operation.receiveReturned) })
}

func (operation *exactReceiverTestOperation) releaseRemoteError() {
	operation.releaseOnce.Do(func() { close(operation.releaseRemote) })
}

func newReceiverShutdownHarness(
	t *testing.T,
	gateRemote bool,
) (*receiverHarness, *exactReceiverTestOperation) {
	t.Helper()
	var operation *exactReceiverTestOperation
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		operation = newExactReceiverTestOperation(
			signaling.operation.(*receiverTestOperation),
			gateRemote,
		)
		signaling.operation = operation
	})
	t.Cleanup(func() {
		operation.releaseRemoteError()
		_ = harness.attempt.Close()
	})
	receiveTest(t, operation.receiveEntered)
	return harness, operation
}

func TestReceiverAttemptConcurrentCloseTerminatesExactOperationOnce(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	start := make(chan struct{})
	results := make(chan error, concurrentReceiverCloses)
	var closes sync.WaitGroup
	for range concurrentReceiverCloses {
		closes.Go(func() {
			<-start
			results <- harness.attempt.Close()
		})
	}
	close(start)
	closes.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent receiver Close: %v", err)
		}
	}
	if calls := operation.terminateCalls.Load(); calls != 1 {
		t.Fatalf("exact signaling operation Terminate calls=%d, want 1", calls)
	}
	outcome := harness.attempt.Outcome()
	if harness.attempt.Err() != nil || outcome.TransitionAuthority() != ReceiverTerminalLocal ||
		!outcome.LocallyCanceled() {
		t.Fatalf("locally closed receiver outcome=%+v", outcome)
	}
}

func TestReceiverAttemptShutdownUnblocksChannelBeforeJoiningDrain(t *testing.T) {
	harness := newReceiverTeardownGateHarness(t)
	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()

	assertReceiverTeardownReachedDrainAfterPeerShutdown(t, harness)
	harness.gates.releaseDrain()
	if err := receiveTest(t, closed); err != nil {
		t.Fatalf("receiver Close after gated drain: %v", err)
	}
	if calls := harness.operation.terminateCalls.Load(); calls != 1 {
		t.Fatalf("exact signaling termination calls=%d, want 1", calls)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalLocal || !outcome.LocallyCanceled() ||
		outcome.RetainedCause() != nil {
		t.Fatalf("sealed local teardown outcome=%+v", outcome)
	}
	assertReceiverTeardownTrace(t, receiveTest(t, harness.traces))
}

func TestReceiverAttemptRemoteTerminalStillJoinsNormalChannelDrain(t *testing.T) {
	harness := newReceiverTeardownGateHarness(t)
	harness.operation.remoteError <- sessionruntime.ErrOperationMissing

	assertReceiverTeardownReachedDrainAfterPeerShutdown(t, harness)
	harness.gates.releaseDrain()
	receiveTest(t, harness.attempt.Done())
	if err := harness.attempt.Close(); err != nil {
		t.Fatalf("idempotent Close after remote terminal: %v", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRemote || outcome.RetainedCause() != nil ||
		!errors.Is(outcome.Cause(), sessionruntime.ErrOperationMissing) {
		t.Fatalf("sealed remote teardown outcome=%+v", outcome)
	}
	assertReceiverTeardownTrace(t, receiveTest(t, harness.traces))
}

func TestReceiverAttemptRemoteFinalOperationMissingIsBenign(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	operation.remoteError <- sessionruntime.ErrOperationMissing
	receiveTest(t, harness.attempt.Done())
	if err := harness.attempt.Close(); err != nil {
		t.Fatalf("idempotent Close after remote retirement: %v", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRemote || outcome.RetainedCause() != nil ||
		!errors.Is(outcome.Cause(), sessionruntime.ErrOperationMissing) {
		t.Fatalf("remote retirement outcome=%+v", outcome)
	}
	if calls := operation.terminateCalls.Load(); calls != 1 {
		t.Fatalf("retired exact operation Terminate calls=%d, want 1", calls)
	}
}

func TestReceiverAttemptLocalOperationMissingIsBenignOnlyForLocalOwner(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	operation.terminateCause = sessionruntime.ErrOperationMissing

	if err := harness.attempt.Close(); err != nil {
		t.Fatalf("local exact-operation retirement: %v", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalLocal || !outcome.LocallyCanceled() ||
		!errors.Is(outcome.Cause(), sessionruntime.ErrOperationMissing) ||
		!containsReceiverBenignCause(outcome.BenignComponents(), ReceiverBenignLocalOperationMissing) {
		t.Fatalf("local operation-missing outcome=%+v", outcome)
	}
}

func TestReceiverAttemptRuntimeTerminationIsRetained(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	operation.remoteDecision = receiverTestDecision(ReceiverTerminalRuntime)
	operation.remoteError <- sessionruntime.ErrRuntimeClosed
	receiveTest(t, harness.attempt.Done())

	if err := harness.attempt.Close(); !errors.Is(err, sessionruntime.ErrRuntimeClosed) {
		t.Fatalf("runtime termination residual=%v", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRuntime ||
		outcome.Disposition() != ReceiverDispositionSessionUnavailable || outcome.LocallyCanceled() ||
		!errors.Is(outcome.RetainedCause(), sessionruntime.ErrRuntimeClosed) ||
		len(outcome.BenignComponents()) != 0 {
		t.Fatalf("runtime termination outcome=%+v", outcome)
	}
}

func TestReceiverAttemptUnexpectedAuthenticatedKindIsNotBenign(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	operation.remoteError <- protocolsession.ErrUnknownMessageKind
	receiveTest(t, harness.attempt.Done())

	if err := harness.attempt.Close(); !errors.Is(err, protocolsession.ErrUnknownMessageKind) {
		t.Fatalf("unexpected authenticated kind residual=%v", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRemote || outcome.RetainedCause() == nil ||
		!errors.Is(outcome.RetainedCause(), protocolsession.ErrUnknownMessageKind) ||
		outcome.LocallyCanceled() {
		t.Fatalf("unexpected authenticated kind outcome=%+v", outcome)
	}
	if !containsReceiverCauseClass(outcome.RetainedCauseClasses(), ReceiverCauseProtocol) {
		t.Fatalf("unexpected authenticated kind trace classes=%v", outcome.RetainedCauseClasses())
	}
}

func TestReceiverAttemptGenuineRemoteFailureRemainsVisibleToClose(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	remoteFailure := errors.New("authenticated remote peer failure")
	operation.remoteError <- remoteFailure
	receiveTest(t, harness.attempt.Done())
	if err := harness.attempt.Close(); !errors.Is(err, remoteFailure) {
		t.Fatalf("Close hid genuine remote failure: %v", err)
	}
	if !errors.Is(harness.attempt.Err(), remoteFailure) {
		t.Fatalf("receiver Err hid genuine remote failure: %v", harness.attempt.Err())
	}
}

func TestReceiverAttemptJoinedCancellationRetainsFailure(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	genuineFailure := errors.New("signaling cancellation cleanup failed")
	operation.terminateCause = errors.Join(context.Canceled, genuineFailure)

	err := harness.attempt.Close()
	if !errors.Is(err, genuineFailure) || errors.Is(err, context.Canceled) {
		t.Fatalf("Close residual=%v, want only genuine cancellation failure", err)
	}
	outcome := harness.attempt.Outcome()
	if !errors.Is(outcome.Cause(), context.Canceled) || !errors.Is(outcome.Cause(), genuineFailure) ||
		!errors.Is(outcome.RetainedCause(), genuineFailure) {
		t.Fatalf("joined cancellation outcome=%+v", outcome)
	}
}

func TestReceiverAttemptJoinedRemoteMissingRetainsConflict(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	genuineConflict := errors.New("remote final conflicted with retained signaling state")
	operation.remoteError <- errors.Join(sessionruntime.ErrOperationMissing, genuineConflict)
	receiveTest(t, harness.attempt.Done())

	err := harness.attempt.Close()
	if !errors.Is(err, genuineConflict) || errors.Is(err, sessionruntime.ErrOperationMissing) {
		t.Fatalf("Close residual=%v, want only genuine remote conflict", err)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRemote ||
		!errors.Is(outcome.Cause(), sessionruntime.ErrOperationMissing) ||
		!errors.Is(outcome.RetainedCause(), genuineConflict) {
		t.Fatalf("joined remote final outcome=%+v", outcome)
	}
}

func TestReceiverAttemptConcurrentLocalCloseAndRemoteFinalRetainsLosingCause(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, true)
	remoteFailure := errors.New("remote final failed after local shutdown began")
	localCleanupFailure := errors.New("losing local termination cleanup failed")
	operation.terminateCause = localCleanupFailure
	operation.remoteError <- errors.Join(sessionruntime.ErrOperationMissing, remoteFailure)
	receiveTest(t, operation.remoteTerminal)

	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before joining the exact receive worker: %v", err)
	default:
	}
	operation.releaseRemoteError()
	if err := receiveTest(t, closed); !errors.Is(err, remoteFailure) ||
		!errors.Is(err, localCleanupFailure) {
		t.Fatalf("concurrent remote final residual=%v", err)
	}
	if calls := operation.terminateCalls.Load(); calls != 1 {
		t.Fatalf("concurrent exact operation Terminate calls=%d, want 1", calls)
	}
	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalRemote ||
		!errors.Is(outcome.RetainedCause(), remoteFailure) ||
		!errors.Is(outcome.RetainedCause(), localCleanupFailure) {
		t.Fatalf("concurrent terminal ownership outcome=%+v", outcome)
	}
}

func TestReceiverAttemptTerminateFailureIsRetained(t *testing.T) {
	harness, operation := newReceiverShutdownHarness(t, false)
	cancelFailure := errors.New("exact signaling object cancellation failed")
	operation.terminateCause = cancelFailure

	if err := harness.attempt.Close(); !errors.Is(err, cancelFailure) {
		t.Fatalf("Close lost exact operation Terminate failure: %v", err)
	}
	if !errors.Is(harness.attempt.Err(), cancelFailure) {
		t.Fatalf("Err lost exact operation Terminate failure: %v", harness.attempt.Err())
	}
}

func TestReceiverAttemptSignalingOpenCancellationRetainsFailure(t *testing.T) {
	openEntered := make(chan struct{})
	genuineFailure := errors.New("signaling open failed while shutdown raced")
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.open = func(
			ctx context.Context,
			_ ReceiverSignalingOperationBinding,
			_ []byte,
		) (ReceiverSignalingOperation, error) {
			close(openEntered)
			<-ctx.Done()
			return nil, errors.Join(ctx.Err(), genuineFailure)
		}
	})
	receiveTest(t, openEntered)

	err := harness.attempt.Close()
	if !errors.Is(err, genuineFailure) || errors.Is(err, ErrNegotiation) ||
		errors.Is(err, context.Canceled) {
		t.Fatalf("signaling open residual=%v", err)
	}
}

func TestReceiverAttemptUnexpectedOpenCancellationIsNotBenign(t *testing.T) {
	harness := newReceiverHarness(t, func(_ *ReceiverFactoryConfig, signaling *receiverTestSignaling) {
		signaling.open = func(
			context.Context,
			ReceiverSignalingOperationBinding,
			[]byte,
		) (ReceiverSignalingOperation, error) {
			return nil, context.Canceled
		}
	})
	receiveTest(t, harness.attempt.Done())

	outcome := harness.attempt.Outcome()
	if outcome.TransitionAuthority() != ReceiverTerminalLocal || outcome.LocallyCanceled() ||
		outcome.TransitionProvenance() != ReceiverProvenanceLocalNegotiationFailure ||
		outcome.Disposition() != ReceiverDispositionFallbackAllowed ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceLocalNegotiationFailure ||
		!errors.Is(outcome.RetainedCause(), ErrNegotiation) ||
		!errors.Is(outcome.RetainedCause(), context.Canceled) ||
		errors.Is(outcome.RetainedCause(), ErrProtocol) ||
		outcome.HasRetainedCauseClass(ReceiverCauseProtocol) {
		t.Fatalf("unexpected live-context cancellation outcome=%+v", outcome)
	}
}

func TestReceiverWorkflowCompletionRetainsCompetingLocalCancelCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrEventCapacity)
	workflow, contextCanceled := receiverWorkflowCompletion(
		receiverWorkflowDiagnostic(context.Canceled),
		ctx,
		receiverOperationDecision(ReceiverTerminalLocal, ReceiverProvenanceLocalContextEnded),
	)
	if !contextCanceled || !errors.Is(workflow.cause, context.Canceled) ||
		!errors.Is(workflow.cause, ErrEventCapacity) {
		t.Fatalf("workflow completion=%+v context_canceled=%v", workflow, contextCanceled)
	}
	classified := classifyReceiverCause(workflow.cause, receiverCausePolicy{contextCanceled: true})
	if !errors.Is(classified.retained, ErrEventCapacity) ||
		errors.Is(classified.retained, context.Canceled) {
		t.Fatalf("workflow completion residual=%+v", classified)
	}
}
