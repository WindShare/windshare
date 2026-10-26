package sessioncore

import (
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func observeSenderTerminalSend(
	observer sessionobs.SenderTerminalSendObserver,
	sessionID protocolsession.ProtocolSessionID,
	lane sessioncontract.LaneIdentity,
	completion protocolsession.SendCompletion,
	naturallyRetired bool,
) {
	if observer == nil {
		return
	}
	observation := sessionobs.SenderTerminalSendObserved{
		ProtocolSessionID: sessionID,
		Lane:              lane,
		Settled:           completion.Settled,
		Decision:          sessionobs.SenderTerminalSendDecisionFailed,
	}
	switch {
	case !completion.Settled:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportUnsettled
	case completion.TransportDisposition == framechannel.SendRetired:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportRetired
		observation.Decision = sessionobs.SenderTerminalSendDecisionNaturalRetirement
	case completion.TransportDisposition == framechannel.SendRejected:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportRejected
	case completion.TransportDisposition == framechannel.SendAccepted:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportAccepted
	default:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportNotReached
	}
	switch completion.Outcome {
	case protocolsession.SendOutcomeTransportConfirmed:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeDelivered
		observation.Decision = sessionobs.SenderTerminalSendDecisionDelivered
	case protocolsession.SendOutcomeDropped:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeDropped
	case protocolsession.SendOutcomeUnknown:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeUnknown
	}
	if naturallyRetired && observation.Decision != sessionobs.SenderTerminalSendDecisionDelivered {
		observation.Decision = sessionobs.SenderTerminalSendDecisionNaturalRetirement
	}
	// Terminal-send evidence is diagnostic consequence data. Observer failures
	// cannot change the already-owned receipt settlement or stop result.
	func() {
		defer func() { _ = recover() }()
		observer.ObserveSenderTerminalSend(observation)
	}()
}
