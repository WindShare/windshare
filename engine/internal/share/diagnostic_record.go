package share

import (
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (value Observation) DiagnosticRecord() diagnostics.Record {
	fields := diagnostics.Fields{"stage": diagnostics.Text(string(value.Milestone))}
	if value.Failure != nil {
		fields["failure"] = diagnostics.Text(value.Failure.Error())
	}
	correlation := diagnostics.Fields{}
	if !value.ShareInstance.IsZero() {
		correlation["share_instance_id"] = diagnostics.Identity(value.ShareInstance[:])
	}
	if value.Session != (protocolsession.ProtocolSessionID{}) {
		correlation["protocol_session_id"] = diagnostics.Identity(value.Session[:])
	}
	return diagnostics.New("share_workflow", fields).Correlate(correlation)
}
