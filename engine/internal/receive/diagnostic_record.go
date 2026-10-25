package receive

import (
	"strconv"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/diagnostics"
)

func (v GenerationChanged) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("receive_generation_changed", diagnostics.Fields{
		"previous_protocol_session_id": diagnostics.Identity(v.Previous[:]),
	}).Correlate(diagnostics.Fields{"protocol_session_id": diagnostics.Identity(v.Current[:]), "receive_operation_id": diagnostics.Identity(v.Operation[:]), "transfer_job_id": diagnostics.Identity(v.Job[:])})
}
func (v AdmissionObserved) DiagnosticRecord() diagnostics.Record {
	f := diagnostics.Fields{"trigger": diagnostics.Text(string(v.Trigger)), "terminal_owner": diagnostics.Text(string(v.TerminalOwner))}
	if v.Cause != nil {
		f["cause"] = diagnostics.Text(v.Cause.Error())
	}
	return diagnostics.New("receive_admission_decision", f).Correlate(diagnostics.Fields{"protocol_session_id": diagnostics.Identity(v.Session[:]), "receive_operation_id": diagnostics.Identity(v.Operation[:]), "transfer_job_id": diagnostics.Identity(v.Job[:])})
}
func (v RelayContentClaimed) DiagnosticRecord() diagnostics.Record {
	return diagnostics.New("receive_relay_content_claimed", diagnostics.Fields{"trigger": diagnostics.Text(string(v.Trigger))}).Correlate(diagnostics.Fields{"protocol_session_id": diagnostics.Identity(v.Session[:]), "receive_operation_id": diagnostics.Identity(v.Operation[:]), "transfer_job_id": diagnostics.Identity(v.Job[:])})
}
func (o getObservation) peerTerminated(v v2peer.ReceiverTerminationTrace, reason ReceiverLocalStopReason) {
	if o.control.Diagnostics.Enabled() {
		record := v.DiagnosticRecord().With(diagnostics.Fields{"local_stop_reason": diagnostics.Text(reason.DiagnosticName())})
		record = record.Correlate(diagnostics.Fields{"protocol_session_id": diagnostics.Identity(o.session[:])})
		o.control.Diagnostics.Emit(record)
	}
	o.emit(PeerTerminated{Value: v, LocalStop: reason})
}
func (v ReceiverLocalStopReason) DiagnosticName() string {
	switch v {
	case ReceiverLocalStopNone:
		return "none"
	case ReceiverLocalStopCaller:
		return "caller"
	case ReceiverLocalStopOutputAdmission:
		return "output_admission"
	case ReceiverLocalStopRuntimeSessionFailure:
		return "runtime_session_failure"
	case ReceiverLocalStopNormalCompletion:
		return "normal_completion"
	default:
		return "unknown_" + strconv.FormatUint(uint64(v), 10)
	}
}
