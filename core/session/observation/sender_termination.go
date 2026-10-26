package sessionobs

import (
	"github.com/windshare/windshare/core/diagnosticerror"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type SenderSessionTerminalTrigger string

const (
	SenderSessionTerminalTriggerGracefulStop   SenderSessionTerminalTrigger = "graceful_stop"
	SenderSessionTerminalTriggerForcedClose    SenderSessionTerminalTrigger = "forced_close"
	SenderSessionTerminalTriggerPeerTerminal   SenderSessionTerminalTrigger = "peer_terminal"
	SenderSessionTerminalTriggerPathsExhausted SenderSessionTerminalTrigger = "paths_exhausted"
	SenderSessionTerminalTriggerRuntimeFailed  SenderSessionTerminalTrigger = "runtime_failed"
)

type SenderSessionTerminalProvenance string

const (
	SenderSessionTerminalProvenanceNormalStop     SenderSessionTerminalProvenance = "normal_stop"
	SenderSessionTerminalProvenanceCallerStop     SenderSessionTerminalProvenance = "caller_stop"
	SenderSessionTerminalProvenanceRemoteClose    SenderSessionTerminalProvenance = "remote_close"
	SenderSessionTerminalProvenanceLaneRetirement SenderSessionTerminalProvenance = "lane_retirement"
	SenderSessionTerminalProvenanceLocalFault     SenderSessionTerminalProvenance = "local_fault"
)

// SenderSessionTerminated is the single causal root for a sender session. The
// closed pair owns lifecycle classification; Failure is independent, immutable
// diagnostic evidence captured by the winning failure decision.
type SenderSessionTerminated struct {
	ProtocolSessionID protocolsession.ProtocolSessionID
	Trigger           SenderSessionTerminalTrigger
	Provenance        SenderSessionTerminalProvenance
	Failure           diagnosticerror.Snapshot
}

func (terminated SenderSessionTerminated) Valid() bool {
	if terminated.ProtocolSessionID.IsZero() {
		return false
	}
	return validSenderSessionTerminalPair(terminated.Trigger, terminated.Provenance)
}

func validSenderSessionTerminalPair(
	trigger SenderSessionTerminalTrigger,
	provenance SenderSessionTerminalProvenance,
) bool {
	switch trigger {
	case SenderSessionTerminalTriggerGracefulStop:
		return provenance == SenderSessionTerminalProvenanceNormalStop
	case SenderSessionTerminalTriggerForcedClose:
		return provenance == SenderSessionTerminalProvenanceCallerStop
	case SenderSessionTerminalTriggerPeerTerminal:
		return provenance == SenderSessionTerminalProvenanceRemoteClose
	case SenderSessionTerminalTriggerPathsExhausted:
		return provenance == SenderSessionTerminalProvenanceLaneRetirement
	case SenderSessionTerminalTriggerRuntimeFailed:
		return provenance == SenderSessionTerminalProvenanceLocalFault
	default:
		return false
	}
}

type SenderSessionTerminalObserver interface {
	ObserveSenderSessionTerminated(SenderSessionTerminated)
}

type SenderSessionTerminalObserverFunc func(SenderSessionTerminated)

func (function SenderSessionTerminalObserverFunc) ObserveSenderSessionTerminated(
	terminated SenderSessionTerminated,
) {
	if function != nil {
		function(terminated)
	}
}

func (value SenderTerminalSendObserved) DiagnosticRecord() diag.Record {
	return diag.New(terminalSendEventName, diag.Fields{"settled": diag.Bool(value.Settled), "transport_disposition": diag.Text(string(value.TransportDisposition)),
		"outcome": diag.Text(string(value.Outcome)), "decision": diag.Text(string(value.Decision))}).Correlate(diag.Fields{
		"protocol_session_id": diag.Identity(value.ProtocolSessionID[:]), "lane_id": diag.Number(float64(value.Lane.ID)), "lane_epoch": diag.Number(float64(value.Lane.Epoch))})
}

func (value SenderSessionTerminated) DiagnosticRecord() diag.Record {
	return diag.New(sessionTerminatedEventName, diag.Fields{"trigger": diag.Text(string(value.Trigger)), "provenance": diag.Text(string(value.Provenance)),
		"failure": diag.Failure(value.Failure)}).Correlate(diag.Fields{"protocol_session_id": diag.Identity(value.ProtocolSessionID[:])})
}
