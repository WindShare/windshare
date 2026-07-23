package sessionruntime

import (
	"context"
	"sync"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationCall struct {
	id            protocolsession.OperationID
	messages      chan operationResponse
	laneMu        sync.Mutex
	lane          LaneIdentity
	stateMu       sync.Mutex
	closed        bool
	done          chan struct{}
	candidateSend chan struct{}

	generation protocolsession.OperationGeneration
	authority  protocolsession.OutboundOperationPermit
	request    protocolsession.Message
	replay     protocolsession.OutboundReplayPermit

	// The admitted continuation bound is operation identity metadata, not live
	// authority. Retaining it after close prevents shutdown timing from changing
	// the public contract of an operation that was already returned to a caller.
	maximumContinuations int
	hasContinuationLimit bool

	authenticatedViolation        protocolsession.AuthenticatedOperationViolation
	authenticatedViolationHandler func(protocolsession.AuthenticatedOperationViolation)
}

func (call *operationCall) observeAuthenticatedOperationViolation(
	violation protocolsession.AuthenticatedOperationViolation,
) {
	if call == nil || !validAuthenticatedOperationViolationCode(violation.Code()) {
		return
	}
	call.stateMu.Lock()
	if validAuthenticatedOperationViolationCode(call.authenticatedViolation.Code()) {
		call.stateMu.Unlock()
		return
	}
	call.authenticatedViolation = violation
	handler := call.authenticatedViolationHandler
	call.stateMu.Unlock()
	if handler != nil {
		handler(violation)
	}
}

func (call *operationCall) registerAuthenticatedOperationViolationHandler(
	handler func(protocolsession.AuthenticatedOperationViolation),
) error {
	if call == nil || handler == nil {
		return ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.authenticatedViolationHandler != nil {
		call.stateMu.Unlock()
		return ErrOperationMissing
	}
	call.authenticatedViolationHandler = handler
	violation := call.authenticatedViolation
	call.stateMu.Unlock()
	if validAuthenticatedOperationViolationCode(violation.Code()) {
		handler(violation)
	}
	return nil
}

func validAuthenticatedOperationViolationCode(
	code protocolsession.AuthenticatedOperationViolationCode,
) bool {
	switch code {
	case protocolsession.AuthenticatedOperationViolationMalformedFailure,
		protocolsession.AuthenticatedOperationViolationMalformedPeerControl,
		protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer,
		protocolsession.AuthenticatedOperationViolationConflictingFinal,
		protocolsession.AuthenticatedOperationViolationContinuationAuthority:
		return true
	default:
		return false
	}
}

func (call *operationCall) acquireCandidateSend(
	ctx context.Context,
	lifetime context.Context,
) (func(), error) {
	if call == nil || ctx == nil || lifetime == nil {
		return nil, ErrOperationMissing
	}
	call.stateMu.Lock()
	if call.closed {
		call.stateMu.Unlock()
		return nil, ErrOperationMissing
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
		return nil, ErrRuntimeClosed
	case <-done:
		return nil, ErrOperationMissing
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
		return nil, ErrRuntimeClosed
	case <-done:
		release()
		return nil, ErrOperationMissing
	default:
		return release, nil
	}
}

func (call *operationCall) doneChannel() <-chan struct{} {
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

func (call *operationCall) enqueue(response operationResponse) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return nil
	}
	select {
	case call.messages <- response:
		return nil
	default:
		return ErrOperationOverflow
	}
}

func (call *operationCall) close() {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return
	}
	call.closed = true
	call.generation = protocolsession.OperationGeneration{}
	call.authority = protocolsession.OutboundOperationPermit{}
	call.request = protocolsession.Message{}
	call.replay = protocolsession.OutboundReplayPermit{}
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

type operationResponse struct {
	message    protocolsession.Message
	generation protocolsession.OperationGeneration
}

func (call *operationCall) setAuthority(
	generation protocolsession.OperationGeneration,
	authority protocolsession.OutboundOperationPermit,
) bool {
	maximumContinuations, hasContinuationLimit := generation.MaximumContinuations()
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return false
	}
	call.generation = generation
	call.authority = authority
	call.maximumContinuations = maximumContinuations
	call.hasContinuationLimit = hasContinuationLimit
	return true
}

func (call *operationCall) operationAuthority() (
	protocolsession.OperationGeneration,
	protocolsession.OutboundOperationPermit,
) {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.generation, call.authority
}

func (call *operationCall) continuationLimit() (int, bool) {
	if call == nil {
		return 0, false
	}
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	return call.maximumContinuations, call.hasContinuationLimit
}

func (call *operationCall) setRequestReplay(
	request protocolsession.Message,
	permit protocolsession.OutboundReplayPermit,
) bool {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed || permit.IsZero() {
		return false
	}
	call.request = request
	call.replay = permit
	return true
}

func (call *operationCall) queueRequestReplay(writer *protocolsession.SessionWriter) error {
	call.stateMu.Lock()
	defer call.stateMu.Unlock()
	if call.closed {
		return ErrOperationMissing
	}
	if call.replay.IsZero() {
		return nil
	}
	if writer == nil {
		return ErrLaneUnavailable
	}
	if _, err := writer.TryControlReplay(call.request, call.replay); err != nil {
		return err
	}
	// Queue admission does not prove physical delivery. Retaining the exact,
	// generation-bound capability lets every later lane migration re-establish
	// the offer dependency before publishing a candidate on that lane.
	return nil
}
