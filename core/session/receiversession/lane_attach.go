package receiversession

import (
	"context"
	"github.com/windshare/windshare/core/session"
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
		return protocolsession.LaneGrant{}, session.ErrRuntimeClosed
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
		return protocolsession.LaneGrant{}, session.ErrOperationMissing
	}
	unsigned := message.Body()
	return protocolsession.DecodeLaneGrant(unsigned, call.ID())
}

func (runtime *Runtime) DetachLane(identity session.LaneIdentity) bool {
	return runtime != nil && runtime.core.DetachLane(identity)
}

// RetireLaneIfCovered atomically checks that the named successor (or any
// alternative, if nil) can carry control traffic before retiring a usable lane.
func (runtime *Runtime) RetireLaneIfCovered(identity session.LaneIdentity, successor *session.LaneIdentity) bool {
	return runtime != nil && runtime.core != nil && runtime.core.RetireLaneIfCovered(identity, successor)
}

// UsableLaneDone lets connectivity publish a newly attached lane only while
// its exact epoch can carry authenticated traffic.
func (runtime *Runtime) UsableLaneDone(identity session.LaneIdentity) (<-chan struct{}, bool) {
	if runtime == nil || runtime.core == nil {
		return nil, false
	}
	return runtime.core.UsableLaneDone(identity)
}

func (runtime *Runtime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.core.AttachedLanes()
}
