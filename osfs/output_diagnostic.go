package osfs

import (
	"strconv"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func (value FilesystemOutputTraceOperation) DiagnosticName() string {
	names := [...]string{
		"", "identity_evidence", "feature_probe_completed", "checkpoint_namespace_opened", "native_lock",
		"session_opened", "checkpoint_reconciled", "runtime_decision",
	}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemOutputRuntimeComponent) DiagnosticName() string {
	names := [...]string{"", "session", "directory", "file", "checkpoint"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemOutputRuntimeOperation) DiagnosticName() string {
	names := [...]string{"", "open_direct_tree", "acquire_operation_lease", "reconcile_checkpoints", "admit_directory", "finalize_directory", "begin_file", "write_range", "checkpoint_file", "commit_file", "pause_file", "retire_file", "pause_tree", "finalize_tree", "materialize_directory", "create_owned_file", "recover_file", "publish_file", "quarantine_file", "admit_destination", "first_write", "cleanup", "delivery_receipt", "release_live_file", "live_retention"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemOutputRuntimeDecision) DiagnosticName() string {
	names := [...]string{"", "validated", "reserved", "coalesced", "rejected", "rolled_back", "admitted", "active", "sealed", "settled", "ambiguous", "draining", "closed", "succeeded", "reconciled", "collision", "no_change", "needs_attention", "isolated_failure", "cleanup_pending"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemCheckpointDecision) DiagnosticName() string {
	names := [...]string{"", "absent", "exact", "revision_conflict", "ownership_conflict", "invalid"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemOutputNativeLockScope) DiagnosticName() string {
	names := [...]string{"", "session"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (value FilesystemOutputNativeLockMilestone) DiagnosticName() string {
	names := [...]string{"", "acquired", "contended", "acquire_failed", "released", "release_reported_failure"}
	if int(value) >= len(names) {
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
	return names[value]
}
func (v FilesystemOutputTrace) DiagnosticRecord() diagnostics.Record {
	f := diagnostics.Fields{
		"operation":             diagnostics.Text(v.Operation.DiagnosticName()),
		"identity_scheme":       diagnostics.Text(string(v.IdentityScheme)),
		"root_disposition":      diagnostics.Text(string(v.RootOpenDisposition)),
		"runtime_component":     diagnostics.Text(v.RuntimeComponent.DiagnosticName()),
		"runtime_operation":     diagnostics.Text(v.RuntimeOperation.DiagnosticName()),
		"runtime_decision":      diagnostics.Text(v.RuntimeDecision.DiagnosticName()),
		"checkpoint_decision":   diagnostics.Text(v.CheckpointDecision.DiagnosticName()),
		"native_lock_scope":     diagnostics.Text(v.NativeLockScope.DiagnosticName()),
		"native_lock_milestone": diagnostics.Text(v.NativeLockMilestone.DiagnosticName()),
		"operation_id":          diagnostics.Uint(v.OperationID), "claim_id": diagnostics.Uint(v.ClaimID),
		"failed":              diagnostics.Bool(v.Failed),
		"failure_stage":       diagnostics.Text(v.FailureStage.String()),
		"reconciliation_step": diagnostics.Text(v.ReconciliationStep.String()),
		"native_error_class":  diagnostics.Text(v.NativeErrorClass.String()),
		"node_claim_count":    diagnostics.Uint(v.NodeClaimCount), "directory_claim_count": diagnostics.Uint(v.DirectoryClaimCount),
		"file_claim_count": diagnostics.Uint(v.FileClaimCount), "active_file_claim_count": diagnostics.Uint(v.ActiveFileClaimCount),
		"reserved_file_slot_count": diagnostics.Uint(v.ReservedFileSlotCount), "directory_metadata_bytes": diagnostics.Uint(v.DirectoryMetadataBytes),
		"checkpoint_record_count":  diagnostics.Uint(v.CheckpointRecordCount),
		"retained_live_file_count": diagnostics.Uint(v.RetainedLiveFileCount),
	}
	if v.RuntimeOperation == FilesystemOutputRuntimeWriteRange || v.RuntimeOperation == FilesystemOutputRuntimeFirstWrite {
		f["write_offset"] = diagnostics.Uint(v.WriteOffset)
		f["write_length"] = diagnostics.Uint(v.WriteLength)
	}
	if v.CheckpointAttentionCount != 0 {
		f["checkpoint_attention_code"] = diagnostics.Text(v.CheckpointAttentionCode)
		f["checkpoint_attention_count"] = diagnostics.Uint(v.CheckpointAttentionCount)
	}
	if v.Failed {
		f["fault"] = diagnostics.Object(diagnostics.Fields{"domain": diagnostics.Number(float64(v.FaultDomain)), "scope": diagnostics.Number(float64(v.NormalizedFaultScope)), "code": diagnostics.Number(float64(v.NormalizedFaultCode))})
	}
	if v.ExecutionMode != 0 {
		c := v.DestinationCapabilities
		f["capabilities"] = diagnostics.Object(diagnostics.Fields{
			"mode":               diagnostics.Text(v.ExecutionMode.String()),
			"safe_publish":       diagnosticCapability(c.SafePublish()),
			"operation_recovery": diagnosticCapability(c.OperationRecovery()),
			"range_recovery":     diagnosticCapability(c.RangeRecovery()),
			"crash_cleanup":      diagnosticCapability(c.CrashCleanup()),
		})
	}
	correlation := diagnostics.Fields{}
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
	return diagnostics.New("filesystem_output", f).Correlate(correlation)
}
func diagnosticCapability(v outputcap.CapabilityEvidence) diagnostics.Value {
	return diagnostics.Object(diagnostics.Fields{"supported": diagnostics.Bool(v.Supported()), "reason": diagnostics.Text(v.Reason().String())})
}
