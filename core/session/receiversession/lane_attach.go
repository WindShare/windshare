package receiversession

import (
	"context"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// RequestLane obtains the one-use sender-signed grant before connectivity opens
// or attaches a new physical channel.
func (runtime *Runtime) RequestLane(
	ctx context.Context,
	requestedLaneID uint32,
) (sessioncontract.LaneAttachmentGrant, error) {
	if runtime == nil {
		return sessioncontract.LaneAttachmentGrant{}, sessioncontract.ErrRuntimeClosed
	}
	body, err := sessioncore.EncodeLaneAttachRequest(requestedLaneID)
	if err != nil {
		return sessioncontract.LaneAttachmentGrant{}, err
	}
	call, err := runtime.rpc.Begin(ctx, protocolsession.MessageLaneAttach, body)
	if err != nil {
		return sessioncontract.LaneAttachmentGrant{}, err
	}
	defer func() { _ = runtime.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	message, err := runtime.rpc.Await(ctx, call)
	if err != nil {
		return sessioncontract.LaneAttachmentGrant{}, err
	}
	if message.Kind() == protocolsession.MessageOperationError {
		return sessioncontract.LaneAttachmentGrant{}, remoteOperationError(message)
	}
	if message.Kind() != protocolsession.MessageLaneAttach {
		return sessioncontract.LaneAttachmentGrant{}, sessioncontract.ErrOperationMissing
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return sessioncontract.LaneAttachmentGrant{}, err
	}
	return sessioncore.DecodeLaneGrant(unsigned, call.ID())
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
