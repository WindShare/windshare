package relayv2

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type LifecycleStage string

const (
	LifecycleTerminalReserved   LifecycleStage = "terminal_reserved"
	LifecycleSendAdmitted       LifecycleStage = "send_admitted"
	LifecycleSendRejected       LifecycleStage = "send_rejected"
	LifecycleSendRolledBack     LifecycleStage = "send_rolled_back"
	LifecycleRetirementDeferred LifecycleStage = "retirement_deferred"
	LifecycleRetired            LifecycleStage = "retired"
	LifecycleTerminalSettled    LifecycleStage = "terminal_settled"
	LifecycleLinkRetiring       LifecycleStage = "link_retiring"
	LifecycleLinkClosed         LifecycleStage = "link_closed"
)

type LifecycleRetirementSource string

const (
	LifecycleRetirementLocalClose     LifecycleRetirementSource = "local_close"
	LifecycleRetirementTerminal       LifecycleRetirementSource = "terminal"
	LifecycleRetirementRelaySession   LifecycleRetirementSource = "relay_session"
	LifecycleRetirementLinkClose      LifecycleRetirementSource = "link_close"
	LifecycleRetirementLinkFailure    LifecycleRetirementSource = "link_failure"
	LifecycleRetirementIngressFailure LifecycleRetirementSource = "ingress_failure"
)

type LifecycleCause string

const (
	LifecycleCauseNone            LifecycleCause = "none"
	LifecycleCauseCanceled        LifecycleCause = "canceled"
	LifecycleCauseDeadline        LifecycleCause = "deadline"
	LifecycleCauseFrameBounds     LifecycleCause = "frame_bounds"
	LifecycleCauseEgressOverflow  LifecycleCause = "egress_overflow"
	LifecycleCauseIngressOverflow LifecycleCause = "ingress_overflow"
	LifecycleCauseSessionRetired  LifecycleCause = "session_retired"
	LifecycleCauseProtocol        LifecycleCause = "protocol"
	LifecycleCauseClosed          LifecycleCause = "closed"
	LifecycleCauseTransport       LifecycleCause = "transport"
)

// LifecycleTrace carries stable correlation and enum-like decisions without
// leaking provider error text or frame contents into operational logs.
type LifecycleTrace struct {
	LinkID           uint64
	RelaySessionID   v2.RelaySessionID
	OperationID      uint64
	Stage            LifecycleStage
	Terminal         bool
	Disposition      framechannel.SendDisposition
	RetirementSource LifecycleRetirementSource
	Cause            LifecycleCause
	DrainCause       LifecycleCause
}

// LifecycleTracer runs after transition locks are released so observation
// cannot alter the winner; implementations should return promptly.
type LifecycleTracer interface {
	TraceRelayLifecycle(LifecycleTrace)
}

type LifecycleTraceFunc func(LifecycleTrace)

func (function LifecycleTraceFunc) TraceRelayLifecycle(event LifecycleTrace) {
	if function != nil {
		function(event)
	}
}

func lifecycleCause(err error) LifecycleCause {
	switch {
	case err == nil:
		return LifecycleCauseNone
	case errors.Is(err, context.Canceled):
		return LifecycleCauseCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return LifecycleCauseDeadline
	case errors.Is(err, ErrFrameBounds):
		return LifecycleCauseFrameBounds
	case errors.Is(err, ErrEgressOverflow):
		return LifecycleCauseEgressOverflow
	case errors.Is(err, ErrIngressOverflow):
		return LifecycleCauseIngressOverflow
	case errors.Is(err, ErrSessionRetired):
		return LifecycleCauseSessionRetired
	case errors.Is(err, ErrProtocol):
		return LifecycleCauseProtocol
	case errors.Is(err, ErrClosed):
		return LifecycleCauseClosed
	default:
		return LifecycleCauseTransport
	}
}
