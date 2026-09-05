package v2peer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const (
	attemptEventReserve    = 16
	failureDeliveryTimeout = 2 * time.Second

	peerNegotiationFailureMessage = "Peer negotiation failed"
	peerTimeoutFailureMessage     = "Peer negotiation timed out"
	peerCandidateFailureMessage   = "ICE candidate exchange failed"
	peerCandidateLimitMessage     = "ICE candidate limit exceeded"
	peerAdmissionFailureMessage   = "Peer channel admission failed"
	peerAttemptCancelledMessage   = "Peer attempt was cancelled"
	peerRuntimeStoppedMessage     = "Sender peer runtime stopped"
	peerUnexpectedFailureMessage  = "Peer attempt failed unexpectedly"
)

var (
	errCandidateLimit   = errors.New("ICE candidate limit exceeded")
	errChannelAdmission = errors.New("peer DataChannel admission failed")
	errAnswerDropped    = errors.New("peer answer operation was retired before delivery")
	errCandidateDropped = errors.New("peer candidate operation was retired before delivery")
)

type peerOperationRejection struct {
	code    uint16
	message string
	cause   error
}

func (rejection *peerOperationRejection) Error() string { return rejection.message }
func (rejection *peerOperationRejection) Unwrap() error { return rejection.cause }

type peerAttemptConfig struct {
	factory    *Factory
	session    sessionruntime.SenderPeerSession
	operation  protocolsession.OperationID
	generation protocolsession.OperationGeneration
	offer      v2signal.Offer
	onDone     func(*peerAttempt, error)
}

type attemptEventKind uint8

const (
	attemptRemoteCandidate attemptEventKind = iota + 1
	attemptLocalCandidate
	attemptDataChannel
	attemptDataChannelOpen
	attemptAdmission
	attemptChannelDone
	attemptConnectionFailed
	attemptOperationCanceled
)

type attemptEvent struct {
	kind             attemptEventKind
	candidate        v2signal.Candidate
	raw              *pion.DataChannel
	lane             sessionruntime.LaneIdentity
	admission        sessionruntime.SenderPeerAdmissionResult
	err              error
	completed        chan struct{}
	admissionContext context.Context
}

type peerAttempt struct {
	config   peerAttemptConfig
	recorder *senderAttemptRecorder
	events   chan attemptEvent
	inboxMu  sync.Mutex
	closed   bool

	cancelMu                 sync.Mutex
	cancel                   context.CancelCauseFunc
	attached                 atomic.Bool
	operationCancelRequested atomic.Bool
	done                     chan struct{}

	phases          *peerPhaseLifecycle
	admissionMu     sync.Mutex
	admissionPhase  attemptAdmissionPhase
	admissionResult attemptEvent
	admissionDone   chan struct{}
	admissionCancel context.CancelCauseFunc
}

func newPeerAttempt(config peerAttemptConfig) *peerAttempt {
	return &peerAttempt{
		config: config,
		recorder: newSenderAttemptRecorder(
			config.factory, config.session.ProtocolSessionID(), config.offer.Binding, config.operation,
		),
		events: make(chan attemptEvent, config.factory.maxCandidates*2+attemptEventReserve),
		done:   make(chan struct{}), admissionDone: make(chan struct{}),
		phases: newPeerPhaseLifecycle(
			config.factory.phaseTimers,
			config.factory.negotiationBudget,
			config.factory.admissionBudget,
		),
	}
}

func (attempt *peerAttempt) binding() v2signal.Binding { return attempt.config.offer.Binding }

func (attempt *peerAttempt) operation() peerOperation {
	return peerOperation{id: attempt.config.operation, generation: attempt.config.generation}
}

func (attempt *peerAttempt) start(parent context.Context, work *sync.WaitGroup) {
	ctx, cancel := context.WithCancelCause(parent)
	attempt.cancelMu.Lock()
	attempt.cancel = cancel
	attempt.cancelMu.Unlock()
	go func() {
		defer work.Done()
		result := attempt.run(ctx)
		attempt.closeInbox()
		close(attempt.done)
		cancel(result)
		attempt.config.onDone(attempt, result)
	}()
}

func (attempt *peerAttempt) stop(reason error) {
	attempt.cancelMu.Lock()
	cancel := attempt.cancel
	attempt.cancelMu.Unlock()
	if cancel != nil {
		cancel(reason)
	}
}

func (attempt *peerAttempt) remoteCandidate(
	candidate v2signal.Candidate,
) (bool, error) {
	attempt.inboxMu.Lock()
	if attempt.closed {
		attempt.inboxMu.Unlock()
		return false, nil
	}
	overflow := false
	select {
	case attempt.events <- attemptEvent{
		kind: attemptRemoteCandidate, candidate: candidate,
	}:
	default:
		overflow = true
	}
	attempt.inboxMu.Unlock()
	if overflow {
		attempt.stop(ErrEventCapacity)
		return false, ErrEventCapacity
	}
	return true, nil
}

func (attempt *peerAttempt) cancelOperation(ctx context.Context) error {
	// Cancellation must wake capacity/preparation I/O before the event loop
	// exists. Admitted lanes still use the existing settlement arbitration.
	attempt.operationCancelRequested.Store(true)
	attempt.phases.cancelBeforeAdmission(context.Canceled)
	completed := make(chan struct{})
	attempt.push(attemptEvent{kind: attemptOperationCanceled, completed: completed})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-attempt.done:
		return nil
	case <-completed:
		return nil
	}
}

func (attempt *peerAttempt) push(event attemptEvent) {
	attempt.inboxMu.Lock()
	if attempt.closed {
		attempt.inboxMu.Unlock()
		if event.raw != nil {
			_ = event.raw.Close()
		}
		if event.completed != nil {
			close(event.completed)
		}
		return
	}
	overflow := false
	select {
	case attempt.events <- event:
	default:
		overflow = true
	}
	attempt.inboxMu.Unlock()
	if overflow {
		if event.raw != nil {
			_ = event.raw.Close()
		}
		if event.completed != nil {
			close(event.completed)
		}
		attempt.stop(ErrEventCapacity)
	}
}

func (attempt *peerAttempt) closeInbox() {
	attempt.inboxMu.Lock()
	defer attempt.inboxMu.Unlock()
	if attempt.closed {
		return
	}
	attempt.closed = true
	for {
		select {
		case event := <-attempt.events:
			if event.raw != nil {
				_ = event.raw.Close()
			}
			if event.completed != nil {
				close(event.completed)
			}
		default:
			return
		}
	}
}

func (attempt *peerAttempt) finish(
	ctx context.Context,
	primary error,
	cleanup error,
	operationCanceled bool,
) error {
	operationCanceled = operationCanceled || attempt.operationCancelRequested.Load()
	result := errors.Join(primary, cleanup)
	if !attempt.recorder.admitted() {
		attempt.recorder.fail(attemptFailure(result, primary, operationCanceled))
	}
	if cleanup != nil {
		attempt.config.factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	}
	return attempt.deliverFailure(
		ctx,
		result,
		operationCanceled || errors.Is(primary, errAnswerDropped) || errors.Is(primary, errCandidateDropped),
	)
}

type attemptExecution struct {
	attempt *peerAttempt
	ctx     context.Context
	peer    PeerConnection
	channel PeerDataChannel

	children          sync.WaitGroup
	phaseContext      context.Context
	transport         *ownedPeerDataChannel
	openTransition    <-chan struct{}
	localCandidates   int
	remoteCandidates  int
	dataChannelSeen   bool
	signaling         bool
	operationCanceled bool
	terminalAuthority bool
}

func newAttemptExecution(
	attempt *peerAttempt,
	ctx context.Context,
	peer PeerConnection,
) *attemptExecution {
	return &attemptExecution{attempt: attempt, ctx: ctx, peer: peer, signaling: true}
}

func (execution *attemptExecution) registerCallbacks() {
	observePeerICE(execution.peer, execution.attempt.phases)
	accept := candidateAdmission(execution.peer, execution.attempt.config.factory.maxCandidates)
	execution.peer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if !accept(candidate) {
			return
		}
		value := candidate.ToJSON()
		execution.attempt.push(attemptEvent{
			kind: attemptLocalCandidate,
			candidate: v2signal.Candidate{
				Binding: execution.attempt.binding(), Candidate: value.Candidate,
				SDPMid: value.SDPMid, SDPMLineIndex: value.SDPMLineIndex,
				UsernameFragment: value.UsernameFragment,
			},
		})
	})
	execution.peer.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateFailed {
			execution.attempt.push(attemptEvent{
				kind: attemptConnectionFailed, err: errors.New("PeerConnection entered failed state"),
			})
		}
	})
	execution.peer.OnDataChannel(func(raw *pion.DataChannel) {
		execution.attempt.push(attemptEvent{kind: attemptDataChannel, raw: raw})
	})
}

func (execution *attemptExecution) negotiate() error {
	if err := execution.peer.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  execution.attempt.config.offer.SDP,
	}); err != nil {
		return fmt.Errorf("set remote offer: %w", err)
	}
	answer, err := execution.peer.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create local answer: %w", err)
	}
	execution.attempt.recorder.complete(
		SenderAttemptAnswerCreated, execution.candidateCounts(), SenderAttemptObservation{},
	)
	if err := execution.peer.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local answer: %w", err)
	}
	localAnswer := execution.peer.LocalDescription()
	if localAnswer == nil || localAnswer.Type != pion.SDPTypeAnswer {
		return errors.New("PeerConnection did not retain the local answer")
	}
	answerBody, err := v2signal.EncodeAnswer(v2signal.Answer{
		Binding: execution.attempt.binding(), SDP: localAnswer.SDP,
	})
	if err != nil {
		return err
	}
	disposition, err := execution.attempt.config.session.SendPeerControl(
		execution.phaseContext,
		protocolsession.MessagePeerAnswer,
		execution.attempt.config.operation,
		answerBody,
	)
	if err != nil {
		if cause := context.Cause(execution.phaseContext); cause != nil {
			return cause
		}
		return err
	}
	if disposition == protocolsession.OperationDrop {
		return errAnswerDropped
	}
	execution.attempt.recorder.complete(
		SenderAttemptAnswerSent, execution.candidateCounts(), SenderAttemptObservation{},
	)
	return nil
}

func (execution *attemptExecution) close(result error) error {
	execution.attempt.stop(result)
	execution.attempt.phases.terminate(result)
	var teardown peerTransportTeardown
	if execution.transport == nil {
		teardown = teardownPeerTransport(execution.peer, nil)
	} else {
		_ = execution.transport.closeIfUnconsumed()
	}
	execution.children.Wait()
	if execution.transport != nil {
		teardown = execution.transport.teardownSnapshot()
	}
	return teardown.cause()
}

func (execution *attemptExecution) runEvents() error {
	for {
		var terminal bool
		var result error
		select {
		case <-execution.ctx.Done():
			terminal, result = execution.settleLoopTermination(context.Cause(execution.ctx))
		case expiration := <-execution.attempt.phases.expirationEvents():
			terminal, result = execution.handlePhaseExpiration(expiration)
		case event := <-execution.attempt.events:
			done, err := execution.handleEvent(event)
			terminal, result = execution.settleEventTerminal(done, err)
		}
		if terminal {
			return result
		}
	}
}

func (execution *attemptExecution) handlePhaseExpiration(expiration peerPhaseExpiration) (bool, error) {
	// Open and its phase transition are one logical event even when the timer
	// notification is selected first. Reconcile the observable channel state so
	// scheduler ordering cannot turn a timely negotiation into a timeout.
	if expiration.phase == PeerAttemptPhaseNegotiation &&
		execution.channel != nil && execution.openTransition != nil {
		select {
		case <-execution.channel.Opened():
			<-execution.openTransition
			return false, nil
		default:
		}
	}
	expired, settling := execution.attempt.phases.expire(expiration)
	if !expired && !settling {
		return false, nil
	}
	execution.attempt.recorder.phaseDeadlineExpired(expiration.phase)
	return execution.settleLoopTermination(expiration.cause)
}

func (execution *attemptExecution) settleLoopTermination(cause error) (bool, error) {
	owned, admitted, admissionErr := execution.settleAdmissionAtTerminal(cause)
	if !owned {
		return true, cause
	}
	if admitted && execution.ctx.Err() == nil {
		return false, nil
	}
	return true, admissionErr
}

func (execution *attemptExecution) handleEvent(event attemptEvent) (bool, error) {
	switch event.kind {
	case attemptRemoteCandidate:
		return false, execution.addRemoteCandidate(event.candidate)
	case attemptLocalCandidate:
		return false, execution.sendLocalCandidate(event.candidate)
	case attemptDataChannel:
		return false, execution.startDataChannel(event.raw)
	case attemptDataChannelOpen:
		if event.err != nil {
			return true, event.err
		}
		execution.phaseContext = event.admissionContext
		execution.attempt.recorder.dataChannelOpened(execution.candidateCounts())
		return false, nil
	case attemptAdmission:
		owned, admitted, err := execution.acceptAdmission(event)
		if !owned {
			return false, nil
		}
		execution.terminalAuthority = !admitted
		return !admitted, err
	case attemptChannelDone:
		return true, execution.channelClosed(event.err)
	case attemptConnectionFailed:
		return true, event.err
	case attemptOperationCanceled:
		return execution.cancelOperation(event)
	default:
		return true, ErrProtocol
	}
}

func (execution *attemptExecution) settleEventTerminal(done bool, cause error) (bool, error) {
	if !done && cause == nil {
		return false, nil
	}
	if execution.terminalAuthority {
		execution.terminalAuthority = false
		return true, cause
	}
	// Event producers are deliberately not an allow-list of terminal races. Any
	// event that would leave the loop must first settle an in-flight core
	// admission, including signaling errors introduced after DataChannel open.
	owned, _, admissionErr := execution.settleAdmissionAtTerminal(cause)
	if owned {
		return true, admissionErr
	}
	return true, cause
}

func (execution *attemptExecution) addRemoteCandidate(
	candidate v2signal.Candidate,
) error {
	if !execution.signaling {
		return nil
	}
	if execution.remoteCandidates >= execution.attempt.config.factory.maxCandidates {
		return errCandidateLimit
	}
	if err := execution.peer.AddICECandidate(candidateInit(candidate)); err != nil {
		return fmt.Errorf("add remote ICE candidate: %w", err)
	}
	execution.remoteCandidates++
	execution.attempt.recorder.recordCandidateCounts(execution.candidateCounts())
	return nil
}

func (execution *attemptExecution) sendLocalCandidate(candidate v2signal.Candidate) error {
	if !execution.signaling {
		return nil
	}
	if execution.localCandidates >= execution.attempt.config.factory.maxCandidates {
		return errCandidateLimit
	}
	body, err := v2signal.EncodeCandidate(candidate)
	if err != nil {
		return err
	}
	disposition, err := execution.attempt.config.session.SendPeerControl(
		execution.ctx,
		protocolsession.MessagePeerCandidate,
		execution.attempt.config.operation,
		body,
	)
	if err != nil {
		return err
	}
	if disposition == protocolsession.OperationDrop {
		return errCandidateDropped
	}
	execution.localCandidates++
	execution.attempt.recorder.recordCandidateCounts(execution.candidateCounts())
	return nil
}

func (execution *attemptExecution) channelClosed(channelErr error) error {
	if execution.operationCanceled {
		return nil
	}
	if execution.attempt.attached.Load() && channelErr == nil {
		return nil
	}
	return errors.Join(
		errChannelAdmission,
		channelErr,
		errors.New("peer channel closed before operation cancellation"),
	)
}

func (execution *attemptExecution) candidateCounts() SenderCandidateCounts {
	return SenderCandidateCounts{
		LocalEmitted: uint32(execution.localCandidates), RemoteAccepted: uint32(execution.remoteCandidates),
	}
}

func (execution *attemptExecution) cancelOperation(event attemptEvent) (bool, error) {
	execution.operationCanceled = true
	execution.signaling = false
	if event.completed != nil {
		close(event.completed)
	}
	owned, admitted, admissionErr := execution.settleAdmissionAtTerminal(context.Canceled)
	if owned {
		return !admitted, admissionErr
	}
	return !execution.attempt.attached.Load(), nil
}

func candidateInit(candidate v2signal.Candidate) pion.ICECandidateInit {
	return pion.ICECandidateInit{
		Candidate: candidate.Candidate, SDPMid: candidate.SDPMid,
		SDPMLineIndex: candidate.SDPMLineIndex, UsernameFragment: candidate.UsernameFragment,
	}
}
