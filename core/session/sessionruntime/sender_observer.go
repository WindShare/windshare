package sessionruntime

import (
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SenderTerminalTransportDisposition string

const (
	SenderTerminalTransportAccepted   SenderTerminalTransportDisposition = "accepted"
	SenderTerminalTransportNotReached SenderTerminalTransportDisposition = "not_reached"
	SenderTerminalTransportUnsettled  SenderTerminalTransportDisposition = "unsettled"
	SenderTerminalTransportRejected   SenderTerminalTransportDisposition = "rejected_before_acceptance"
	SenderTerminalTransportRetired    SenderTerminalTransportDisposition = "retired_before_acceptance"
)

type SenderTerminalOutcome string

const (
	SenderTerminalOutcomeDelivered SenderTerminalOutcome = "delivered"
	SenderTerminalOutcomeDropped   SenderTerminalOutcome = "dropped"
	SenderTerminalOutcomeUnknown   SenderTerminalOutcome = "unknown"
)

type SenderTerminalDecision string

const (
	SenderTerminalDecisionDelivered         SenderTerminalDecision = "delivered"
	SenderTerminalDecisionNaturalRetirement SenderTerminalDecision = "natural_retirement"
	SenderTerminalDecisionFailed            SenderTerminalDecision = "failed"
)

// SenderTerminalObservation exposes only stable identities and decisions. The
// terminal body, cryptographic material, and provider-specific error text stay
// below this boundary so production logs cannot leak share content or keys.
type SenderTerminalObservation struct {
	ProtocolSessionID    protocolsession.ProtocolSessionID
	Lane                 LaneIdentity
	Settled              bool
	TransportDisposition SenderTerminalTransportDisposition
	Outcome              SenderTerminalOutcome
	Decision             SenderTerminalDecision
}

type SenderTerminalObserver interface {
	ObserveSenderTerminal(SenderTerminalObservation)
}

type SenderTerminalObserverFunc func(SenderTerminalObservation)

func (function SenderTerminalObserverFunc) ObserveSenderTerminal(observation SenderTerminalObservation) {
	if function != nil {
		function(observation)
	}
}

func observeSenderTerminal(
	observer SenderTerminalObserver,
	sessionID protocolsession.ProtocolSessionID,
	lane LaneIdentity,
	completion protocolsession.SendCompletion,
) {
	if observer == nil {
		return
	}
	observation := SenderTerminalObservation{
		ProtocolSessionID: sessionID,
		Lane:              lane,
		Settled:           completion.Settled,
		Decision:          SenderTerminalDecisionFailed,
	}
	switch {
	case !completion.Settled:
		observation.TransportDisposition = SenderTerminalTransportUnsettled
	case completion.TransportDisposition == framechannel.SendRetired:
		observation.TransportDisposition = SenderTerminalTransportRetired
		observation.Decision = SenderTerminalDecisionNaturalRetirement
	case completion.TransportDisposition == framechannel.SendRejected:
		observation.TransportDisposition = SenderTerminalTransportRejected
	case completion.TransportDisposition == framechannel.SendAccepted:
		observation.TransportDisposition = SenderTerminalTransportAccepted
	default:
		observation.TransportDisposition = SenderTerminalTransportNotReached
	}
	switch completion.Outcome {
	case protocolsession.SendOutcomeDelivered:
		observation.Outcome = SenderTerminalOutcomeDelivered
		observation.Decision = SenderTerminalDecisionDelivered
	case protocolsession.SendOutcomeDropped:
		observation.Outcome = SenderTerminalOutcomeDropped
	case protocolsession.SendOutcomeUnknown:
		observation.Outcome = SenderTerminalOutcomeUnknown
	}
	observer.ObserveSenderTerminal(observation)
}
