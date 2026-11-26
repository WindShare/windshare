package receiversession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
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
	for attempt := 0; ; attempt++ {
		call, err := runtime.rpc.Begin(ctx, protocolsession.MessageLaneAttach, body)
		if err != nil {
			return protocolsession.LaneGrant{}, err
		}
		message, err := runtime.rpc.Await(ctx, call)
		_ = runtime.rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort)
		if errors.Is(err, sessioncore.ErrOperationRetry) && attempt < maxOperationWindowRetries && runtime.rpc.Runtime().HasUsableLanes() {
			continue
		}
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

// HasUsableLanes reports whether at least one attached lane can still carry
// new control traffic and authorize non-initial lane admission.
func (runtime *Runtime) HasUsableLanes() bool {
	return runtime != nil && runtime.core != nil && runtime.core.HasUsableLanes()
}

func (runtime *Runtime) AttachedLanes() int {
	if runtime == nil {
		return 0
	}
	return runtime.core.AttachedLanes()
}
