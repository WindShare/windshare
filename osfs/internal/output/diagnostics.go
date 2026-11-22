package output

import (
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const identitySchemePortableV1 = "portable-witness-v1"

// traceEvent carries structured filesystem_output telemetry emitted at key
// milestones (binding, session admission, file recovery, name repair, pause).
type traceEvent struct {
	component          string
	operation          string
	decision           string
	checkpointDecision string
	failed             bool
	failureStage       FailureStage
	nativeClass        NativeErrorClass
	bindingID          [32]byte
	operationID        receivecontract.OperationID
	intentDigest       transfer.ReceiveIntentDigest
	sessionID          transfer.OutputSessionID
	fileID             catalog.FileID
	capabilities       DestinationCapabilities
	previousRevision   content.FileRevision
	currentRevision    content.FileRevision
	recoveryReason     content.RevisionComparisonReason
	reusedBytes        uint64
	retainedBytes      uint64
	logicalPath        string
	physicalPath       string
	nameAttempt        uint32
	nameReused         bool
}

func emitTrace(sink diagnostics.Sink, ev traceEvent) {
	if !sink.Enabled() {
		return
	}
	fields := diagnostics.Fields{
		"operation":          diagnostics.Text("runtime_decision"),
		"identity_scheme":    diagnostics.Text(identitySchemePortableV1),
		"runtime_component":  diagnostics.Text(ev.component),
		"runtime_operation":  diagnostics.Text(ev.operation),
		"runtime_decision":   diagnostics.Text(ev.decision),
		"failed":             diagnostics.Bool(ev.failed),
		"failure_stage":      diagnostics.Text(ev.failureStage.String()),
		"native_error_class": diagnostics.Text(ev.nativeClass.String()),
	}
	if ev.checkpointDecision != "" {
		fields["checkpoint_decision"] = diagnostics.Text(ev.checkpointDecision)
	}
	if ev.recoveryReason != "" {
		fields["recovery_reason"] = diagnostics.Text(string(ev.recoveryReason))
		fields["previous_revision"] = diagnostics.Identity(ev.previousRevision.Bytes())
		fields["current_revision"] = diagnostics.Identity(ev.currentRevision.Bytes())
		fields["reused_bytes"] = diagnostics.Uint(ev.reusedBytes)
		fields["retained_bytes"] = diagnostics.Uint(ev.retainedBytes)
	}
	if ev.operation == "map_name" {
		fields["logical_path"] = diagnostics.Text(ev.logicalPath)
		fields["physical_path"] = diagnostics.Text(ev.physicalPath)
		fields["name_mapping_attempt"] = diagnostics.Uint(uint64(ev.nameAttempt))
		fields["name_mapping_reused"] = diagnostics.Bool(ev.nameReused)
	}
	if ev.capabilities.Valid() {
		fields["capabilities"] = diagnostics.Object(diagnostics.Fields{
			"safe_publish": diagnostics.Object(diagnostics.Fields{
				"supported": diagnostics.Bool(ev.capabilities.SafePublish),
				"reason":    diagnostics.Text(ev.capabilities.SafePublishReason),
			}),
			"operation_recovery": diagnostics.Object(diagnostics.Fields{
				"supported": diagnostics.Bool(ev.capabilities.OperationRecovery),
				"reason":    diagnostics.Text(ev.capabilities.OperationRecoveryReason),
			}),
		})
	}
	correlation := diagnostics.Fields{}
	if ev.bindingID != ([32]byte{}) {
		correlation["destination_binding_id"] = diagnostics.Identity(ev.bindingID[:])
	}
	if !ev.fileID.IsZero() {
		correlation["file_id"] = diagnostics.Identity(ev.fileID.Bytes())
	}
	if !ev.operationID.IsZero() {
		correlation["receive_operation_id"] = diagnostics.Identity(ev.operationID.Bytes())
	}
	if !ev.intentDigest.IsZero() {
		correlation["receive_intent_digest"] = diagnostics.Identity(ev.intentDigest.Bytes())
	}
	if !ev.sessionID.IsZero() {
		correlation["output_session_id"] = diagnostics.Identity(ev.sessionID.Bytes())
	}
	sink.Emit(diagnostics.New("filesystem_output", fields).Correlate(correlation))
}
