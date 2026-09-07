package clievent

import "github.com/windshare/windshare/core/diagnosticerror"

type SenderSessionTerminated struct {
	session    ProtocolSessionID
	trigger    SenderSessionTerminalTrigger
	provenance SenderSessionTerminalProvenance
	failure    diagnosticerror.Snapshot
}

func NewSenderSessionTerminated(
	session ProtocolSessionID,
	trigger SenderSessionTerminalTrigger,
	provenance SenderSessionTerminalProvenance,
	failure diagnosticerror.Snapshot,
) (SenderSessionTerminated, error) {
	if !session.Valid() || !validSenderSessionTerminalPair(trigger, provenance) {
		return SenderSessionTerminated{}, ErrInvalidEvent
	}
	return SenderSessionTerminated{
		session: session, trigger: trigger, provenance: provenance, failure: failure,
	}, nil
}

func validSenderSessionTerminalPair(
	trigger SenderSessionTerminalTrigger,
	provenance SenderSessionTerminalProvenance,
) bool {
	switch trigger {
	case SenderSessionTerminalGracefulStop:
		return provenance == SenderSessionTerminalNormalStop
	case SenderSessionTerminalForcedClose:
		return provenance == SenderSessionTerminalCallerStop
	case SenderSessionTerminalPeerTerminal:
		return provenance == SenderSessionTerminalRemoteClose
	case SenderSessionTerminalPathsExhausted:
		return provenance == SenderSessionTerminalLaneRetirement
	case SenderSessionTerminalRuntimeFailed:
		return provenance == SenderSessionTerminalLocalFault
	default:
		return false
	}
}

func (SenderSessionTerminated) event()           {}
func (SenderSessionTerminated) Command() Command { return CommandShare }
func (SenderSessionTerminated) Level() Level     { return LevelDebug }
func (value SenderSessionTerminated) ProtocolSessionID() ProtocolSessionID {
	return value.session
}
func (value SenderSessionTerminated) Trigger() SenderSessionTerminalTrigger {
	return value.trigger
}
func (value SenderSessionTerminated) Provenance() SenderSessionTerminalProvenance {
	return value.provenance
}
func (value SenderSessionTerminated) Accept(visitor Visitor) error {
	return acceptSenderSessionTerminated(visitor, value)
}

func (value SenderSessionTerminated) FailureSnapshot() diagnosticerror.Snapshot {
	return value.failure
}
