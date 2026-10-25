package webrtc

import (
	diag "github.com/windshare/windshare/core/diagnostics"
)

const lifecycleEventName = "webrtc_lifecycle"

func (v LifecycleTrace) DiagnosticRecord() diag.Record {
	f := diag.Fields{"channel_id": diag.Uint(v.ChannelID), "operation": diag.Text(string(v.Operation)),
		"transition": diag.Text(string(v.Transition)), "state": diag.Text(v.State.DiagnosticName()),
		"terminal_state": diag.Text(string(v.Terminal)), "cause": diag.Text(string(v.Cause))}
	if v.OperationID != 0 {
		f["send_operation_id"] = diag.Uint(v.OperationID)
	}
	if v.Disposition != 0 {
		f["disposition"] = diag.Text(v.Disposition.DiagnosticName())
	}
	if v.Dropped != 0 {
		f["dropped"] = diag.Uint(v.Dropped)
	}
	return diag.New(lifecycleEventName, f)
}
