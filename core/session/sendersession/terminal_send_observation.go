package sendersession

import (
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func observeSenderTerminalSend(
	observer sessionobs.SenderTerminalSendObserver,
	event sessioncore.TerminalSendObservation,
) {
	if observer == nil {
		return
	}
	observation := sessionobs.SenderTerminalSendObserved{
		ProtocolSessionID: event.ProtocolSessionID,
		Lane:              event.Lane,
		Settled:           event.Settled,
		Decision:          sessionobs.SenderTerminalSendDecisionFailed,
	}
	switch {
	case !event.Settled:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportUnsettled
	case event.TransportDisposition == framechannel.SendRetired:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportRetired
		observation.Decision = sessionobs.SenderTerminalSendDecisionNaturalRetirement
	case event.TransportDisposition == framechannel.SendRejected:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportRejected
	case event.TransportDisposition == framechannel.SendAccepted:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportAccepted
	default:
		observation.TransportDisposition = sessionobs.SenderTerminalSendTransportNotReached
	}
	switch event.Outcome {
	case protocolsession.SendOutcomeTransportConfirmed:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeDelivered
		observation.Decision = sessionobs.SenderTerminalSendDecisionDelivered
	case protocolsession.SendOutcomeDropped:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeDropped
	case protocolsession.SendOutcomeUnknown:
		observation.Outcome = sessionobs.SenderTerminalSendOutcomeUnknown
	}
	if event.NaturallyRetired && observation.Decision != sessionobs.SenderTerminalSendDecisionDelivered {
		observation.Decision = sessionobs.SenderTerminalSendDecisionNaturalRetirement
	}
	// Terminal-send evidence is diagnostic consequence data. Observer failures
	// cannot change the already-owned receipt settlement or stop result.
	func() {
		defer func() { _ = recover() }()
		observer.ObserveSenderTerminalSend(observation)
	}()
}
