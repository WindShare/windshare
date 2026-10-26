package sessionobs

import (
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SenderTerminalSendTransportDisposition string

const (
	SenderTerminalSendTransportAccepted   SenderTerminalSendTransportDisposition = "accepted"
	SenderTerminalSendTransportNotReached SenderTerminalSendTransportDisposition = "not_reached"
	SenderTerminalSendTransportUnsettled  SenderTerminalSendTransportDisposition = "unsettled"
	SenderTerminalSendTransportRejected   SenderTerminalSendTransportDisposition = "rejected_before_acceptance"
	SenderTerminalSendTransportRetired    SenderTerminalSendTransportDisposition = "retired_before_acceptance"
)

type SenderTerminalSendOutcome string

const (
	SenderTerminalSendOutcomeDelivered SenderTerminalSendOutcome = "delivered"
	SenderTerminalSendOutcomeDropped   SenderTerminalSendOutcome = "dropped"
	SenderTerminalSendOutcomeUnknown   SenderTerminalSendOutcome = "unknown"
)

type SenderTerminalSendDecision string

const (
	SenderTerminalSendDecisionDelivered         SenderTerminalSendDecision = "delivered"
	SenderTerminalSendDecisionNaturalRetirement SenderTerminalSendDecision = "natural_retirement"
	SenderTerminalSendDecisionFailed            SenderTerminalSendDecision = "failed"
)

// SenderTerminalSendObserved exposes only stable identities and decisions. The
// terminal body, cryptographic material, and provider-specific error text stay
// below this boundary so production logs cannot leak share content or keys.
type SenderTerminalSendObserved struct {
	ProtocolSessionID    protocolsession.ProtocolSessionID
	Lane                 session.LaneIdentity
	Settled              bool
	TransportDisposition SenderTerminalSendTransportDisposition
	Outcome              SenderTerminalSendOutcome
	Decision             SenderTerminalSendDecision
}

type SenderTerminalSendObserver interface {
	ObserveSenderTerminalSend(SenderTerminalSendObserved)
}

type SenderTerminalSendObserverFunc func(SenderTerminalSendObserved)

func (function SenderTerminalSendObserverFunc) ObserveSenderTerminalSend(observation SenderTerminalSendObserved) {
	if function != nil {
		function(observation)
	}
}
