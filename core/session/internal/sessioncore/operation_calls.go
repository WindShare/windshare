package sessioncore

import (
	"context"
	"sync"
	"time"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
)

type Call struct {
	id                    protocolsession.OperationID
	messages              chan operationResponse
	laneMu                sync.Mutex
	lane                  session.LaneIdentity
	stateMu               sync.Mutex
	lastResponseLane      session.LaneIdentity
	requestCarrierRetired <-chan struct{}
	closed                bool
	done                  chan struct{}
	candidateSend         chan struct{}

	op          protocolsession.Operation
	request     protocolsession.Message
	hasReplay   bool
	requests    requestlane.Call
	requestKind protocolsession.MessageKind

	traceEnabled           bool
	traceStarted           time.Time
	traceDeadlineMillis    uint64
	traceHasDeadline       bool
	traceUsableAtSelection uint32
	traceHasSend           bool
	traceSendStatus        sessionobs.SendStatus
	traceResponseCount     uint64
	traceResponseKind      protocolsession.MessageKind
	traceHasResponse       bool
	traceHasFinalResponse  bool
	traceCause             sessionobs.ProtocolOperationCause
	traceBlockWait         contentflow.BlockWaitTimeout
	traceEmitted           bool
	traceOwner             *TerminalOwner

	maximumContinuations int
	hasContinuationLimit bool

	authenticatedViolation protocolsession.AuthenticatedOperationViolation
	peerOperation          OperationObserver
}

func (call *Call) ID() protocolsession.OperationID { return call.id }

func (call *Call) Operation() protocolsession.Operation {
	if call == nil {
		return protocolsession.Operation{}
	}
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.op
}

func (call *Call) ContinuationLimit() (int, bool) {
	if call == nil {
		return 0, false
	}
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.maximumContinuations, call.hasContinuationLimit
}

func (call *Call) LastResponseLane() session.LaneIdentity {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.lastResponseLane
}

func (call *Call) bindRequestCarrier(retired <-chan struct{}) {
	switch call.requestKind {
	case protocolsession.MessagePeerOffer, protocolsession.MessageRequestBlocks, protocolsession.MessageRenewLease:
		return
	}
	call.stateMu.Lock()
	call.requestCarrierRetired = retired
	call.stateMu.Unlock()
}

func (call *Call) controlCarrierRetired() <-chan struct{} {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.requestCarrierRetired
}

func (call *Call) acceptResponse(response operationResponse) (protocolsession.Message, bool) {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed || !call.op.Same(response.op) {
		return protocolsession.Message{}, false
	}
	call.lastResponseLane = response.lane
	return response.message, true
}

func (call *Call) observeAuthenticatedOperationViolation(
	violation protocolsession.AuthenticatedOperationViolation,
) {
	if call == nil || !violation.Valid() {
		return
	}
	call.stateMu.Lock()
	if call.authenticatedViolation.Valid() {
		call.stateMu.Unlock()
		return
	}
	call.authenticatedViolation = violation
	operation := call.peerOperation
	call.stateMu.Unlock()
	if operation != nil {
		operation.ObserveAuthenticatedOperationViolation(violation)
	}
}

func (call *Call) BindOperationObserver(operation OperationObserver) error {
	if call == nil || operation == nil {
		return session.ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.peerOperation != nil {
		call.stateMu.Unlock()
		return session.ErrOperationMissing
	}
	call.peerOperation = operation
	operation.BindTerminalOwner(call.claimProtocolTerminationLocked())
	violation := call.authenticatedViolation
	call.stateMu.Unlock()
	if violation.Valid() {
		operation.ObserveAuthenticatedOperationViolation(violation)
	}
	return nil
}

func (call *Call) acquireCandidateSend(
	ctx context.Context,
	lifetime context.Context,
) (func(), error) {
	if call == nil || ctx == nil || lifetime == nil {
		return nil, session.ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.closed {
		call.stateMu.Unlock()
		return nil, session.ErrOperationMissing
	}
	if call.candidateSend == nil {
		call.candidateSend = make(chan struct{}, 1)
		call.candidateSend <- struct{}{}
	}
	gate := call.candidateSend
	done := call.done
	if done == nil {
		done = make(chan struct{})
		call.done = done
	}
	call.stateMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lifetime.Done():
		return nil, session.ErrRuntimeClosed
	case <-done:
		return nil, session.ErrOperationMissing
	case <-gate:
	}
	var once sync.Once
	release := func() { once.Do(func() { gate <- struct{}{} }) }
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-lifetime.Done():
		release()
		return nil, session.ErrRuntimeClosed
	case <-done:
		release()
		return nil, session.ErrOperationMissing
	default:
		return release, nil
	}
}

func (call *Call) doneChannel() <-chan struct{} {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.done == nil {
		call.done = make(chan struct{})
		if call.closed {
			close(call.done)
		}
	}
	return call.done
}

func (call *Call) enqueue(response operationResponse) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return nil
	}
	call.traceResponse(response.message.Kind())
	return call.enqueueLocked(response)
}

type authenticatedFailureSource struct {
	observations      observationstream.Producer[sessionobs.ProtocolObservation]
	observedAt        time.Time
	protocolSessionID protocolsession.ProtocolSessionID
	lane              session.LaneIdentity
	hasLane           bool
}

func (call *Call) enqueueAuthenticatedFailure(
	response operationResponse,
	source authenticatedFailureSource,
) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return nil
	}
	call.traceResponse(response.message.Kind())
	call.traceAuthenticatedFailure(response.message, source)
	return call.enqueueLocked(response)
}

func (call *Call) enqueueLocked(response operationResponse) error {
	select {
	case call.messages <- response:
		return nil
	default:
		return session.ErrOperationOverflow
	}
}

func (call *Call) traceResponse(kind protocolsession.MessageKind) {
	if !call.traceEnabled {
		return
	}
	if call.traceResponseCount != ^uint64(0) {
		call.traceResponseCount++
	}
	call.traceResponseKind = kind
	call.traceHasResponse = true
	if senderResponseFinal(kind) {
		call.traceHasFinalResponse = true
	}
}

func (call *Call) traceAuthenticatedFailure(
	message protocolsession.Message,
	source authenticatedFailureSource,
) {
	if !call.traceEnabled {
		return
	}
	content, ok := sessionobs.ProtocolErrorForAuthenticatedReceive(message)
	if !ok {
		return
	}
	if source.hasLane && !source.observations.IsZero() {
		source.observations.TryPublish(sessionobs.NewReceivedProtocolError(
			sessionobs.ProtocolObservationContext{
				Correlation: sessionobs.ProtocolObservationCorrelation{
					Role:              protocolsession.RoleReceiver,
					ProtocolSessionID: source.protocolSessionID,
					OperationID:       call.id,
					RequestKind:       call.requestKind,
				},
				ObservedAt: source.observedAt,
			},
			content, source.lane,
		))
	}
	call.traceCause = call.traceCause.Merge(sessionobs.ProtocolOperationCauseProtocolFailure)
}

func (call *Call) Close() {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return
	}
	call.closed = true
	call.requests.Close()
	call.op = protocolsession.Operation{}
	call.request = protocolsession.Message{}
	call.hasReplay = false
	if call.done == nil {
		call.done = make(chan struct{})
	}
	close(call.done)
	for {
		select {
		case <-call.messages:
		default:
			return
		}
	}
}

func (call *Call) setAuthority(
	op protocolsession.Operation,
	maximumContinuations int,
	hasContinuationLimit bool,
) bool {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return false
	}
	call.op = op
	call.maximumContinuations = maximumContinuations
	call.hasContinuationLimit = hasContinuationLimit
	return true
}

func (call *Call) setRequestReplay(request protocolsession.Message) {
	call.stateMu.Lock()
	if !call.closed {
		call.request = request
		call.hasReplay = true
	}
	call.stateMu.Unlock()
}

func (call *Call) queueRequestReplay(writer *laneWriter) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return session.ErrOperationMissing
	}
	if !call.hasReplay {
		return nil
	}
	if writer == nil {
		return ErrLaneUnavailable
	}
	return writer.EnqueueControl(call.request, nil)
}

func receiverRequestKind(kind protocolsession.MessageKind) bool {
	switch kind {
	case protocolsession.MessageListChildren, protocolsession.MessageOpenRevisions,
		protocolsession.MessageRenewLease, protocolsession.MessageReleaseLease,
		protocolsession.MessageRequestBlocks, protocolsession.MessageLaneAttach,
		protocolsession.MessagePeerOffer:
		return true
	default:
		return false
	}
}

func senderResponseFinal(kind protocolsession.MessageKind) bool {
	switch kind {
	case protocolsession.MessageCatalogResult, protocolsession.MessageOpenResults,
		protocolsession.MessageLeaseResult, protocolsession.MessageOperationComplete,
		protocolsession.MessageOperationError, protocolsession.MessageOperationRetry,
		protocolsession.MessageLaneAttach:
		return true
	default:
		return false
	}
}
