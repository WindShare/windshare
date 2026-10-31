package transfer

import (
	"time"

	diag "github.com/windshare/windshare/core/diagnostics"
)

const lifecycleEventName = "transfer_lifecycle"

func (v TransferLifecycleTrace) DiagnosticRecord() diag.Record {
	f := diag.Fields{"stage": diag.Text(v.Stage.DiagnosticName()), "receive_operation_id": diag.Identity(v.ReceiveOperationID.Bytes()),
		"transfer_job_id": diag.Identity(v.TransferJobID.Bytes()), "file_selection": diag.Text(v.FileSelection.DiagnosticName()),
		"file_settlement": diag.Text(v.FileSettlement.DiagnosticName()), "tree_settlement": diag.Text(v.DirectTreeSettlement.DiagnosticName()),
		"failed": diag.Bool(v.Failed), "progress": v.Progress.DiagnosticValue()}
	if v.ItemBlockReason != 0 {
		f["item_block_reason"] = diag.Text(v.ItemBlockReason.DiagnosticName())
	}
	if !v.FileID.IsZero() {
		f["file_id"] = diag.Identity(v.FileID.Bytes())
	}
	c := diag.Fields{"protocol_session_id": diag.Identity(v.ProtocolSessionID[:]),
		"receive_operation_id": diag.Identity(v.ReceiveOperationID.Bytes()), "transfer_job_id": diag.Identity(v.TransferJobID.Bytes())}
	if !v.ReceiveIntentDigest.IsZero() {
		c["receive_intent_digest"] = diag.Identity(v.ReceiveIntentDigest.Bytes())
	}
	if !v.OutputSessionID.IsZero() {
		c["output_session_id"] = diag.Identity(v.OutputSessionID.Bytes())
	}
	f["plan_kind"] = diag.Number(float64(v.PlanKind))
	f["discovery"] = diag.Text(v.Discovery.DiagnosticName())
	f["connection_size_class"] = diag.Number(float64(v.ConnectionSizeClass))
	if !v.CapacityWaitID.IsZero() {
		f["capacity"] = diag.Object(diag.Fields{"wait_id": diag.Identity(v.CapacityWaitID.Bytes()), "generation_id": diag.Identity(v.CapacityGeneration.Bytes()),
			"protocol_operation_id": diag.Identity(v.CapacityOperationID[:]), "attempt": diag.Uint(v.CapacityAttempt),
			"hint_ms": diag.Millis(v.CapacityHint), "jitter_ms": diag.Millis(v.CapacityJitter), "delay_ms": diag.Millis(v.CapacityDelay),
			"accumulated_wait_ms": diag.Millis(v.CapacityAccumulated), "active_waiters": diag.Number(float64(v.CapacityActiveWaiters))})
		c["protocol_operation_id"] = diag.Identity(v.CapacityOperationID[:])
	}
	if v.Failed {
		f["interruption"] = diag.Number(float64(v.Interruption))
		if !v.Fault.IsZero() {
			f["fault"] = diag.Object(diag.Fields{"domain": diag.Number(float64(v.Fault.Domain())), "scope": diag.Number(float64(v.Fault.Scope())), "code": diag.Number(float64(v.Fault.Code()))})
		}
	}
	return diag.New(lifecycleEventName, f).Correlate(c)
}
func (v ReceiveProgressSnapshot) DiagnosticValue() diag.Value {
	return diag.Object(diag.Fields{"discovery": diag.Text(v.Discovery.DiagnosticName()), "counters_exact": diag.Bool(v.CountersExact),
		"source_omitted_entries": diag.Uint(v.SourceOmittedEntries),
		"discovered_files":       diag.Uint(v.DiscoveredFiles), "discovered_bytes": diag.Uint(v.DiscoveredBytes),
		"published_files": diag.Uint(v.PublishedFiles), "published_bytes": diag.Uint(v.PublishedBytes),
		"verified_bytes": diag.Uint(v.VerifiedBytes), "previously_published_bytes": diag.Uint(v.PreviouslyPublishedBytes),
		"newly_verified_bytes":   diag.Uint(v.NewlyVerifiedBytes),
		"restart_reusable_bytes": diag.Uint(v.RestartReusableBytes),
		"file_outcomes":          v.FileOutcomes.DiagnosticValue(),
		"capacity_wait": diag.Object(diag.Fields{"active_waiters": diag.Number(float64(v.CapacityWait.ActiveWaiters)),
			"active_since":        diag.Text(v.CapacityWait.ActiveSince.Format(time.RFC3339Nano)),
			"visible_after":       diag.Text(v.CapacityWait.VisibleAfter.Format(time.RFC3339Nano)),
			"accumulated_wait_ms": diag.Millis(v.CapacityWait.AccumulatedWait), "attempts": diag.Uint(v.CapacityWait.Attempts)})})
}

func (v FileOutcomeSummary) DiagnosticValue() diag.Value {
	return diag.Object(diag.Fields{
		"downloaded_files":           diag.Uint(v.DownloadedFiles),
		"resumed_files":              diag.Uint(v.ResumedFiles),
		"previously_published_files": diag.Uint(v.PreviouslyPublishedFiles),
		"paused_files":               diag.Uint(v.PausedFiles),
		"collision_files":            diag.Uint(v.CollisionFiles),
		"failed_files":               diag.Uint(v.FailedFiles),
		"item_blocked_files":         diag.Uint(v.ItemBlockedFiles),
		"revision_conflict_files":    diag.Uint(v.RevisionConflictFiles),
		"checkpoint_invalid_files":   diag.Uint(v.CheckpointInvalidFiles),
		"owned_object_unknown_files": diag.Uint(v.OwnedObjectUnknownFiles),
		"modified_time_warnings":     diag.Uint(v.ModifiedTimeWarnings),
	})
}
