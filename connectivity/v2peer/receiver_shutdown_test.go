package v2peer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
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
	traces    chan ReceiverTerminationTrace
}

func newReceiverTeardownGateHarness(t *testing.T) *receiverTeardownGateHarness {
	t.Helper()
	peer := newReceiverTestPeerConnection()
	baseChannel := newReceiverTestChannel()
	gates := newReceiverTeardownGates()
	channel := newReceiverTeardownGateChannel(baseChannel, peer.closed, gates)
	traces := make(chan ReceiverTerminationTrace, 1)
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
		config.OnTermination = func(trace ReceiverTerminationTrace) { traces <- trace }
	})
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
	<-operation.receiveReturned
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

type receiverSessionFailureWrapper struct {
	child       error
	unwrapCalls int
}

type receiverSessionFailureMarkerSpoof struct{}

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

func (*receiverSessionFailureWrapper) Error() string { return "wrapped core session failure" }
func (failure *receiverSessionFailureWrapper) Unwrap() error {
	failure.unwrapCalls++
	return failure.child
}

func (receiverSessionFailureMarkerSpoof) Error() string   { return "spoofed session failure" }
func (receiverSessionFailureMarkerSpoof) SessionFailure() {}

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

func TestReceiverCoreSessionFailureBecomesOwnedProtocolDiagnostic(t *testing.T) {
	unsafeChild := &receiverStatefulWrapperError{}
	coreFailure := transfer.NewSessionFailure(unsafeChild)
	classified := classifyReceiverCause(coreFailure, receiverCausePolicy{})
	if unsafeChild.unwrapCalls != 0 ||
		!errors.Is(classified.retained, errReceiverOpaqueCause) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("core session failure classification=%+v unwrap_calls=%d", classified, unsafeChild.unwrapCalls)
	}
	if _, ok := errors.AsType[*transfer.SessionFailureError](classified.retained); ok {
		t.Fatal("core SessionFailureError escaped the owned receiver residual")
	}
}

func TestReceiverCrossScopeSessionFailureRequiresSealedAuthority(t *testing.T) {
	classified := classifyReceiverCause(
		transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure),
		receiverCausePolicy{},
	)
	if !errors.Is(classified.retained, protocolsession.ErrInvalidOperationFailure) ||
		!containsReceiverCauseClass(classified.classes, ReceiverCauseProtocol) ||
		containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
		t.Fatalf("cross-scope session failure classification=%+v", classified)
	}
	if _, ok := errors.AsType[*transfer.SessionFailureError](classified.retained); ok {
		t.Fatal("cross-scope core wrapper escaped the owned receiver residual")
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

func TestReceiverUntrustedSessionFailureShapesRemainOpaque(t *testing.T) {
	wrapped := &receiverSessionFailureWrapper{
		child: transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure),
	}
	for name, cause := range map[string]error{
		"unknown wrapper": wrapped,
		"marker spoof":    receiverSessionFailureMarkerSpoof{},
	} {
		t.Run(name, func(t *testing.T) {
			classified := classifyReceiverCause(cause, receiverCausePolicy{})
			if classified.retained != errReceiverOpaqueCause ||
				!containsReceiverCauseClass(classified.classes, ReceiverCauseUnknown) {
				t.Fatalf("untrusted session failure classification=%+v", classified)
			}
		})
	}
	if wrapped.unwrapCalls != 0 {
		t.Fatalf("unknown session-failure wrapper unwrapped %d times", wrapped.unwrapCalls)
	}

	var typedNil *transfer.SessionFailureError
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
	diagnostic := transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure)
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
	diagnostic := transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure)
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
	sessionDiagnostic := transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure)
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
