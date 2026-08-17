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
	LifecycleTraceDropped       LifecycleStage = "trace_dropped"
)

type LifecycleRetirementSource string

const (
	LifecycleRetirementNone           LifecycleRetirementSource = "none"
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
	Dropped          uint64
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

func terminalReservedTrace(sessionID v2.RelaySessionID, operationID uint64) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleTerminalReserved, Terminal: true,
		RetirementSource: LifecycleRetirementNone,
		Cause:            LifecycleCauseNone, DrainCause: LifecycleCauseNone,
	}
}

func terminalSendAdmittedTrace(sessionID v2.RelaySessionID, operationID uint64) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleSendAdmitted, Terminal: true,
		Disposition:      framechannel.SendAccepted,
		RetirementSource: LifecycleRetirementNone,
		Cause:            LifecycleCauseNone, DrainCause: LifecycleCauseNone,
	}
}

func acceptedSendFailureTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage:            LifecycleSendAdmitted,
		Disposition:      framechannel.SendAccepted,
		RetirementSource: LifecycleRetirementNone,
		Cause:            cause, DrainCause: LifecycleCauseNone,
	}
}

func sendRejectedTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	terminal bool,
	disposition framechannel.SendDisposition,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleSendRejected, Terminal: terminal, Disposition: disposition,
		RetirementSource: LifecycleRetirementNone,
		Cause:            cause, DrainCause: LifecycleCauseNone,
	}
}

func sendRolledBackTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	terminal bool,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleSendRolledBack, Terminal: terminal,
		Disposition:      framechannel.SendRejected,
		RetirementSource: LifecycleRetirementNone,
		Cause:            cause, DrainCause: LifecycleCauseNone,
	}
}

func retirementDeferredTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	terminal bool,
	source LifecycleRetirementSource,
	cause LifecycleCause,
	drainCause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleRetirementDeferred, Terminal: terminal,
		RetirementSource: source, Cause: cause, DrainCause: drainCause,
	}
}

func retiredTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	terminal bool,
	source LifecycleRetirementSource,
	cause LifecycleCause,
	drainCause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleRetired, Terminal: terminal,
		RetirementSource: source, Cause: cause, DrainCause: drainCause,
	}
}

func terminalSettledTrace(
	sessionID v2.RelaySessionID,
	operationID uint64,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		RelaySessionID: sessionID, OperationID: operationID,
		Stage: LifecycleTerminalSettled, Terminal: true,
		Disposition:      framechannel.SendAccepted,
		RetirementSource: LifecycleRetirementNone,
		Cause:            cause, DrainCause: LifecycleCauseNone,
	}
}

func linkRetiringTrace(
	operationID uint64,
	source LifecycleRetirementSource,
	cause LifecycleCause,
	drainCause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		OperationID: operationID, Stage: LifecycleLinkRetiring,
		RetirementSource: source, Cause: cause, DrainCause: drainCause,
	}
}

func linkClosedTrace(
	operationID uint64,
	source LifecycleRetirementSource,
	cause LifecycleCause,
) LifecycleTrace {
	return LifecycleTrace{
		OperationID: operationID, Stage: LifecycleLinkClosed,
		RetirementSource: source, Cause: cause, DrainCause: LifecycleCauseNone,
	}
}

func traceDroppedSummary(linkID uint64, dropped uint64) LifecycleTrace {
	return LifecycleTrace{
		LinkID: linkID, Stage: LifecycleTraceDropped,
		RetirementSource: LifecycleRetirementNone,
		Cause:            LifecycleCauseNone, DrainCause: LifecycleCauseNone,
		Dropped: dropped,
	}
}
