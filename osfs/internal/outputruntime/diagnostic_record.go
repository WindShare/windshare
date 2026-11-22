package outputruntime

import (
	"strconv"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func (value TraceOperation) DiagnosticName() string {
	names := [...]string{"", "checkpoint_reconciled", "runtime_decision"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}

func (value RuntimeComponent) DiagnosticName() string {
	names := [...]string{"", "session", "directory", "file", "checkpoint"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}

func (value RuntimeOperation) DiagnosticName() string {
	names := [...]string{
		"", "reconcile_checkpoints", "admit_directory", "finalize_directory", "begin_file",
		"write_range", "checkpoint_file", "commit_file", "pause_file", "retire_file",
		"pause_tree", "finalize_tree", "materialize_directory", "create_owned_file",
		"recover_file", "publish_file", "quarantine_file", "admit_destination",
		"first_write", "cleanup", "delivery_receipt", "map_name", "select_operation",
	}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}

func (value RuntimeDecision) DiagnosticName() string {
	names := [...]string{
		"", "validated", "reserved", "coalesced", "rejected", "rolled_back", "admitted",
		"active", "sealed", "settled", "ambiguous", "draining", "closed", "succeeded",
		"reconciled", "collision", "no_change", "needs_attention", "isolated_failure",
		"cleanup_pending", "selection_mismatch",
	}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}

func (value CheckpointDecision) DiagnosticName() string {
	names := [...]string{
		"", "absent", "exact", "revision_conflict", "ownership_conflict",
		"invalid", "metadata_match", "reuse_state_blocked", "revision_evidence_unavailable",
	}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}

func (v Trace) DiagnosticRecord() diagnostics.Record {
	fields := diagnostics.Fields{
		"operation":                diagnostics.Text(v.Operation.DiagnosticName()),
		"identity_scheme":          diagnostics.Text(string(v.IdentityScheme)),
		"runtime_component":        diagnostics.Text(v.RuntimeComponent.DiagnosticName()),
		"runtime_operation":        diagnostics.Text(v.RuntimeOperation.DiagnosticName()),
		"runtime_decision":         diagnostics.Text(v.RuntimeDecision.DiagnosticName()),
		"checkpoint_decision":      diagnostics.Text(v.CheckpointDecision.DiagnosticName()),
		"operation_id":             diagnostics.Uint(v.OperationID),
		"claim_id":                 diagnostics.Uint(v.ClaimID),
		"failed":                   diagnostics.Bool(v.Failed),
		"failure_stage":            diagnostics.Text(v.FailureStage.String()),
		"reconciliation_step":      diagnostics.Text(v.ReconciliationStep.String()),
		"native_error_class":       diagnostics.Text(v.NativeErrorClass.String()),
		"node_claim_count":         diagnostics.Uint(v.NodeClaimCount),
		"directory_claim_count":    diagnostics.Uint(v.DirectoryClaimCount),
		"file_claim_count":         diagnostics.Uint(v.FileClaimCount),
		"active_file_claim_count":  diagnostics.Uint(v.ActiveFileClaimCount),
		"reserved_file_slot_count": diagnostics.Uint(v.ReservedFileSlotCount),
		"directory_metadata_bytes": diagnostics.Uint(v.DirectoryMetadataBytes),
		"checkpoint_record_count":  diagnostics.Uint(v.CheckpointRecordCount),
	}
	if v.RecoveryReason != "" {
		fields["recovery_reason"] = diagnostics.Text(string(v.RecoveryReason))
		fields["previous_revision"] = diagnostics.Identity(v.PreviousRevision.Bytes())
		fields["current_revision"] = diagnostics.Identity(v.CurrentRevision.Bytes())
		fields["reused_bytes"] = diagnostics.Uint(v.ReusedBytes)
		fields["retained_bytes"] = diagnostics.Uint(v.RetainedBytes)
	}
	if v.RuntimeOperation == RuntimeWriteRange || v.RuntimeOperation == RuntimeFirstWrite {
		fields["write_offset"] = diagnostics.Uint(v.WriteOffset)
		fields["write_length"] = diagnostics.Uint(v.WriteLength)
	}
	if v.RuntimeOperation == RuntimeMapName {
		fields["logical_path"] = diagnostics.Text(v.LogicalPath)
		fields["physical_path"] = diagnostics.Text(v.PhysicalPath)
		fields["name_mapping_attempt"] = diagnostics.Uint(uint64(v.NameMappingAttempt))
		fields["name_mapping_reused"] = diagnostics.Bool(v.NameMappingReused)
	}
	if v.CheckpointReclaimedCandidateCount != 0 {
		fields["checkpoint_reclaimed_candidate_count"] = diagnostics.Uint(v.CheckpointReclaimedCandidateCount)
	}
	if v.CheckpointAttentionCount != 0 {
		fields["checkpoint_attention_code"] = diagnostics.Text(v.CheckpointAttentionCode)
		fields["checkpoint_attention_count"] = diagnostics.Uint(v.CheckpointAttentionCount)
	}
	if v.Failed {
		fields["fault"] = diagnostics.Object(diagnostics.Fields{
			"domain": diagnostics.Number(float64(v.FaultDomain)),
			"scope":  diagnostics.Number(float64(v.NormalizedFaultScope)),
			"code":   diagnostics.Number(float64(v.NormalizedFaultCode)),
		})
	}
	if v.DestinationCapabilities.Valid() {
		c := v.DestinationCapabilities
		fields["capabilities"] = diagnostics.Object(diagnostics.Fields{
			"safe_publish":       diagnosticCapability(c.SafePublish()),
			"operation_recovery": diagnosticCapability(c.OperationRecovery()),
		})
	}
	correlation := diagnostics.Fields{}
	if !v.DestinationBindingID.IsZero() {
		correlation["destination_binding_id"] = diagnostics.Identity(v.DestinationBindingID.Bytes())
	}
	if !v.FileID.IsZero() {
		correlation["file_id"] = diagnostics.Identity(v.FileID.Bytes())
	}
	if !v.ReceiveOperationID.IsZero() {
		correlation["receive_operation_id"] = diagnostics.Identity(v.ReceiveOperationID.Bytes())
	}
	if !v.ReceiveIntentDigest.IsZero() {
		correlation["receive_intent_digest"] = diagnostics.Identity(v.ReceiveIntentDigest.Bytes())
	}
	if !v.SessionID.IsZero() {
		correlation["output_session_id"] = diagnostics.Identity(v.SessionID.Bytes())
	}
	return diagnostics.New("filesystem_output", fields).Correlate(correlation)
}

func diagnosticCapability(v outputcap.CapabilityEvidence) diagnostics.Value {
	return diagnostics.Object(diagnostics.Fields{
		"supported": diagnostics.Bool(v.Supported()),
		"reason":    diagnostics.Text(v.Reason().String()),
	})
}
