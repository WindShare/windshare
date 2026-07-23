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
)

var (
	errAttemptTimeout   = errors.New("peer lane admission timed out")
	errCandidateLimit   = errors.New("ICE candidate limit exceeded")
	errChannelAdmission = errors.New("peer DataChannel admission failed")
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
	attemptAdmission
	attemptChannelDone
	attemptConnectionFailed
	attemptOperationCanceled
)

type attemptEvent struct {
	kind      attemptEventKind
	candidate v2signal.Candidate
	raw       *pion.DataChannel
	lane      sessionruntime.LaneIdentity
	err       error
	completed chan struct{}
}

type peerAttempt struct {
	config  peerAttemptConfig
	events  chan attemptEvent
	inboxMu sync.Mutex
	closed  bool

	cancelMu sync.Mutex
	cancel   context.CancelCauseFunc
	attached atomic.Bool
	done     chan struct{}
}

func newPeerAttempt(config peerAttemptConfig) *peerAttempt {
	return &peerAttempt{
		config: config,
		events: make(chan attemptEvent, config.factory.maxCandidates*2+attemptEventReserve),
		done:   make(chan struct{}),
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

func (attempt *peerAttempt) run(ctx context.Context) (result error) {
	var execution *attemptExecution
	defer func() {
		operationCanceled := execution != nil && execution.operationCanceled
		result = attempt.deliverFailure(ctx, result, operationCanceled)
	}()
	peer, err := attempt.config.factory.peerConnections.NewPeerConnection(
		attempt.config.factory.configuration,
	)
	if err != nil || peer == nil {
		return errors.Join(ErrNegotiation, err)
	}
	execution = newAttemptExecution(attempt, ctx, peer)
	defer func() { result = errors.Join(result, execution.close(result)) }()
	execution.registerCallbacks()
	if err := execution.negotiate(); err != nil {
		return err
	}
	execution.startDeadline()
	return execution.runEvents()
}

type attemptExecution struct {
	attempt *peerAttempt
	ctx     context.Context
	peer    PeerConnection
	channel PeerDataChannel

	children          sync.WaitGroup
	timer             *time.Timer
	timeout           <-chan time.Time
	localCandidates   int
	remoteCandidates  int
	dataChannelSeen   bool
	signaling         bool
	operationCanceled bool
}

func newAttemptExecution(
	attempt *peerAttempt,
	ctx context.Context,
	peer PeerConnection,
) *attemptExecution {
	return &attemptExecution{attempt: attempt, ctx: ctx, peer: peer, signaling: true}
}

func (execution *attemptExecution) registerCallbacks() {
	execution.peer.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate == nil {
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
	_, err = execution.attempt.config.session.SendPeerControl(
		execution.ctx,
		protocolsession.MessagePeerAnswer,
		execution.attempt.config.operation,
		answerBody,
	)
	return err
}

func (execution *attemptExecution) startDeadline() {
	execution.timer = time.NewTimer(execution.attempt.config.factory.attemptTimeout)
	execution.timeout = execution.timer.C
}

func (execution *attemptExecution) close(result error) error {
	execution.attempt.stop(result)
	teardown := teardownPeerTransport(execution.peer, execution.channel)
	execution.children.Wait()
	execution.stopDeadline()
	return teardown.cause()
}

func (execution *attemptExecution) runEvents() error {
	for {
		select {
		case <-execution.ctx.Done():
			return context.Cause(execution.ctx)
		case <-execution.timeout:
			return errAttemptTimeout
		case event := <-execution.attempt.events:
			done, err := execution.handleEvent(event)
			if err != nil || done {
				return err
			}
		}
	}
}

func (execution *attemptExecution) handleEvent(event attemptEvent) (bool, error) {
	switch event.kind {
	case attemptRemoteCandidate:
		return false, execution.addRemoteCandidate(event.candidate)
	case attemptLocalCandidate:
		return false, execution.sendLocalCandidate(event.candidate)
	case attemptDataChannel:
		return false, execution.startDataChannel(event.raw)
	case attemptAdmission:
		return false, execution.acceptAdmission(event)
	case attemptChannelDone:
		return true, execution.channelClosed(event.err)
	case attemptConnectionFailed:
		return true, event.err
	case attemptOperationCanceled:
		return execution.cancelOperation(event), nil
	default:
		return true, ErrProtocol
	}
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
	execution.remoteCandidates++
	if err := execution.peer.AddICECandidate(candidateInit(candidate)); err != nil {
		return fmt.Errorf("add remote ICE candidate: %w", err)
	}
	return nil
}

func (execution *attemptExecution) sendLocalCandidate(candidate v2signal.Candidate) error {
	if !execution.signaling {
		return nil
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
	if err != nil || disposition == protocolsession.OperationDrop {
		return err
	}
	execution.localCandidates++
	if execution.localCandidates > execution.attempt.config.factory.maxCandidates {
		return errCandidateLimit
	}
	return nil
}

func (execution *attemptExecution) startDataChannel(raw *pion.DataChannel) error {
	if raw == nil {
		return errors.Join(errChannelAdmission, errors.New("peer delivered a nil DataChannel"))
	}
	if execution.dataChannelSeen {
		_ = raw.Close()
		return errors.Join(errChannelAdmission, errors.New("peer created more than one DataChannel"))
	}
	execution.dataChannelSeen = true
	channel, err := execution.attempt.config.factory.dataChannels.WrapDataChannel(raw)
	if err != nil || channel == nil {
		return errors.Join(errChannelAdmission, err)
	}
	execution.channel = channel
	execution.children.Add(2)
	go func() {
		defer execution.children.Done()
		execution.attempt.admit(execution.ctx, channel)
	}()
	go func() {
		defer execution.children.Done()
		execution.attempt.watchChannel(channel)
	}()
	return nil
}

func (execution *attemptExecution) acceptAdmission(event attemptEvent) error {
	if event.err != nil {
		return errors.Join(errChannelAdmission, event.err)
	}
	if event.lane.ID == 0 || event.lane.Epoch == 0 {
		return errors.Join(errChannelAdmission, errors.New("peer DataChannel admission returned a zero lane"))
	}
	execution.attempt.attached.Store(true)
	execution.stopDeadline()
	return nil
}

func (execution *attemptExecution) channelClosed(channelErr error) error {
	if execution.operationCanceled {
		return nil
	}
	return errors.Join(
		errChannelAdmission,
		channelErr,
		errors.New("peer channel closed before operation cancellation"),
	)
}

func (execution *attemptExecution) cancelOperation(event attemptEvent) bool {
	execution.operationCanceled = true
	execution.signaling = false
	if event.completed != nil {
		close(event.completed)
	}
	return !execution.attempt.attached.Load()
}

func (execution *attemptExecution) stopDeadline() {
	if execution.timer == nil || execution.timeout == nil {
		return
	}
	if !execution.timer.Stop() {
		select {
		case <-execution.timer.C:
		default:
		}
	}
	execution.timeout = nil
}

func (attempt *peerAttempt) deliverFailure(
	ctx context.Context,
	result error,
	operationCanceled bool,
) error {
	if result == nil || operationCanceled || errors.Is(result, context.Canceled) {
		return result
	}
	code, message := peerFailure(result)
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureDeliveryTimeout)
	defer cancel()
	return errors.Join(result, attempt.config.session.FailPeerOperation(
		failureContext,
		attempt.config.operation,
		code,
		message,
	))
}

func peerFailure(err error) (uint16, string) {
	var rejected *peerOperationRejection
	if errors.As(err, &rejected) {
		return rejected.code, rejected.message
	}
	switch {
	case errors.Is(err, errAttemptTimeout):
		return protocolsession.PeerOperationCodeTimeout, peerTimeoutFailureMessage
	case errors.Is(err, errCandidateLimit):
		return protocolsession.PeerOperationCodeCandidates, peerCandidateLimitMessage
	case errors.Is(err, errChannelAdmission):
		return protocolsession.PeerOperationCodeAdmission, peerAdmissionFailureMessage
	default:
		return protocolsession.PeerOperationCodeNegotiation, peerNegotiationFailureMessage
	}
}

func (attempt *peerAttempt) admit(ctx context.Context, channel PeerDataChannel) {
	lane, err := attempt.config.session.AdmitPeerChannel(ctx, channel)
	attempt.push(attemptEvent{kind: attemptAdmission, lane: lane, err: err})
}

func (attempt *peerAttempt) watchChannel(channel PeerDataChannel) {
	<-channel.Done()
	attempt.push(attemptEvent{kind: attemptChannelDone, err: channel.Err()})
}

func candidateInit(candidate v2signal.Candidate) pion.ICECandidateInit {
	return pion.ICECandidateInit{
		Candidate: candidate.Candidate, SDPMid: candidate.SDPMid,
		SDPMLineIndex: candidate.SDPMLineIndex, UsernameFragment: candidate.UsernameFragment,
	}
}
