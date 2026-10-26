package receiversession

import (
	"context"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// RequestLane obtains the one-use sender-signed grant before connectivity opens
// or attaches a new physical channel.
func (runtime *Runtime) RequestLane(
	ctx context.Context,
	requestedLaneID uint32,
) (protocolsession.LaneGrant, error) {
	if runtime == nil {
		return protocolsession.LaneGrant{}, sessioncontract.ErrRuntimeClosed
	}
	body, err := protocolsession.EncodeLaneAttachRequest(requestedLaneID)
	if err != nil {
		return protocolsession.LaneGrant{}, err
	}
	call, err := runtime.rpc.Begin(ctx, protocolsession.MessageLaneAttach, body)
	if err != nil {
		return protocolsession.LaneGrant{}, err
	}
	defer func() { _ = runtime.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := runtime.rpc.Await(ctx, call)
	if err != nil {
		return protocolsession.LaneGrant{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return protocolsession.LaneGrant{}, remoteOperationError(message)
	}
	if message.Kind() != protocolsession.MessageLaneAttach {
		return protocolsession.LaneGrant{}, sessioncontract.ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return protocolsession.LaneGrant{}, err
	}
	return protocolsession.DecodeLaneGrant(unsigned, call.ID())
}

func (runtime *Runtime) DetachLane(identity sessioncontract.LaneIdentity) bool {
	return runtime != nil && runtime.core.DetachLane(identity)
}

func (runtime *Runtime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.core.AttachedLanes()
}
