package relayv2

import (
	diag "github.com/windshare/windshare/core/diagnostics"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const lifecycleEventName = "relay_lifecycle"

func (v LifecycleTrace) DiagnosticRecord() diag.Record {
	f := diag.Fields{"link_id": diag.Uint(v.LinkID), "stage": diag.Text(string(v.Stage)),
		"terminal": diag.Bool(v.Terminal), "retirement_source": diag.Text(string(v.RetirementSource)),
		"cause": diag.Text(string(v.Cause)), "drain_cause": diag.Text(string(v.DrainCause))}
	if v.RelaySessionID != (v2.RelaySessionID{}) {
		f["relay_session_id"] = diag.Identity(v.RelaySessionID[:])
	}
	if v.OperationID != 0 {
		f["send_operation_id"] = diag.Uint(v.OperationID)
	}
	if v.Disposition != 0 {
		f["disposition"] = diag.Text(v.Disposition.DiagnosticName())
	}
	if v.Dropped != 0 {
		f["dropped"] = diag.Uint(v.Dropped)
	}
	if v.HeartbeatRound != 0 {
		f["heartbeat_round"], f["wait_ms"], f["timeout_ms"] = diag.Uint(v.HeartbeatRound), diag.Millis(v.Wait), diag.Millis(v.Timeout)
	}
	return diag.New(lifecycleEventName, f)
}
