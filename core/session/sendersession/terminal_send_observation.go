package sendersession

import (
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
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
		Outcome:           event.Outcome,
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
	if event.Outcome == sessionobs.SenderTerminalSendOutcomeDelivered {
		observation.Decision = sessionobs.SenderTerminalSendDecisionDelivered
	}
	if event.NaturallyRetired && observation.Decision != sessionobs.SenderTerminalSendDecisionDelivered {
		observation.Decision = sessionobs.SenderTerminalSendDecisionNaturalRetirement
	}
	func() {
		defer func() { _ = recover() }()
		observer.ObserveSenderTerminalSend(observation)
	}()
}
