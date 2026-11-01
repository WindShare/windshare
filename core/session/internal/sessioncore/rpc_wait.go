package sessioncore

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// Transport acceptance does not prove that a request or its final response
// reached the peer. The consumer must retire this attempt before retrying.
var ErrRequestCarrierRetired = fmt.Errorf("%w: request carrier retired before response", ErrLaneUnavailable)

func (call *Call) bindRequestCarrier(retired <-chan struct{}) {
	// These operations have owners whose authority survives one carrier. In
	// particular, renewal may finish on another lane; only the lease's confirmed
	// validity bounds that wait. Retrying renewal would change its acceptance window.
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
	if call.closed || !call.generation.Same(response.generation) {
		return protocolsession.Message{}, false
	}
	call.lastResponseLane = response.lane
	return response.message, true
}

func (client *Requests) await(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.awaitCall(ctx, call, true)
}

func (client *Requests) awaitPeer(ctx context.Context, call *Call) (protocolsession.Message, error) {
	return client.awaitCall(ctx, call, false)
}

func (client *Requests) awaitCall(ctx context.Context, call *Call, cancelOnContext bool) (protocolsession.Message, error) {
	if client == nil || client.runtime == nil {
		return protocolsession.Message{}, session.ErrRuntimeClosed
	}
	if call == nil {
		return protocolsession.Message{}, session.ErrOperationMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	carrierRetired := call.controlCarrierRetired()
	for {
		done := call.doneChannel()
		select {
		case <-ctx.Done():
			call.recordProtocolTraceFailure(ctx.Err())
			if !cancelOnContext {
				return protocolsession.Message{}, ctx.Err()
			}
			// Cancellation admission is part of the observable failure. Dropping
			// it would hide failed cancellation behind a benign later cleanup.
			cleanupErr := client.admitCancellation(call, contentflow.CancelReasonTimeout)
			call.recordProtocolTraceCleanup(cleanupErr)
			return protocolsession.Message{}, errors.Join(ctx.Err(), cleanupErr)
		case <-client.runtime.Done():
			call.recordProtocolTraceFailure(session.ErrRuntimeClosed)
			return protocolsession.Message{}, errors.Join(session.ErrRuntimeClosed, client.runtime.Err())
		case <-done:
			call.recordProtocolTraceFailure(session.ErrOperationMissing)
			return protocolsession.Message{}, session.ErrOperationMissing
		case <-carrierRetired:
			// A response already authenticated before retirement remains usable.
			// This also admits a final that migrated to another surviving lane.
			select {
			case response := <-call.messages:
				if message, accepted := call.acceptResponse(response); accepted {
					return message, nil
				}
				continue
			default:
			}
			if client.runtime.Lifetime().Err() != nil {
				call.recordProtocolTraceFailure(session.ErrRuntimeClosed)
				return protocolsession.Message{}, errors.Join(session.ErrRuntimeClosed, client.runtime.Err())
			}
			call.recordProtocolTraceFailure(ErrRequestCarrierRetired)
			return protocolsession.Message{}, ErrRequestCarrierRetired
		case response := <-call.messages:
			if message, accepted := call.acceptResponse(response); accepted {
				return message, nil
			}
		}
	}
}
