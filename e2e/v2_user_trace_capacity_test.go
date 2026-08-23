package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestUserTraceV3RevisionCapacityContract(t *testing.T) {
	runtimeRunID := v3CapacityBase64ID(0x11)
	protocolSessionID := v3CapacityBase64ID(0x22)
	protocolOperationID := v3CapacityBase64ID(0x33)
	transferJobID := v3CapacityBase64ID(0x44)
	waitID := v3CapacityBase64ID(0x55)
	generationID := v3CapacityBase64ID(0x66)
	decisionID := v3CapacityHexID(0x77, v3DigestBytes)
	revisionID := v3CapacityHexID(0x88, v3DigestBytes)
	leaseID := v3CapacityHexID(0x99, v3IdentityBytes)

	progress := v3CapacityProgressVector()
	receiver := []map[string]any{
		v3CapacityTraceRecord(1, "get", "transfer_progress", runtimeRunID, map[string]any{
			"receive_operation_id": protocolOperationID,
			"transfer_job_id":      transferJobID,
			"progress":             progress,
		}),
		v3CapacityTraceRecord(2, "get", "transfer_lifecycle", runtimeRunID, map[string]any{
			"receive_operation_id": protocolOperationID,
			"transfer_job_id":      transferJobID,
			"stage":                "capacity_retry_scheduled",
			"file_selection":       "none",
			"file_settlement":      "none",
			"tree_settlement":      "none",
			"progress":             progress,
			"capacity": map[string]any{
				"wait_id": waitID, "generation_id": generationID,
				"protocol_operation_id": protocolOperationID,
				"attempt":               "2", "hint_ms": "250", "jitter_ms": "9", "delay_ms": "259",
				"accumulated_wait_ms": "400", "active_waiters": 1,
			},
		}),
	}
	v3ReadTraceVectors(t, "get", receiver)

	capacityScope := v3CapacityScopeVector()
	sender := []map[string]any{
		v3CapacityTraceRecord(1, "share", "protocol_operation", runtimeRunID, map[string]any{
			"role": "sender", "stage": "sender_content_decision", "request_kind": "request_file",
			"response_count": "1", "operation_elapsed_ms": "2", "usable_lanes_at_selection": 1,
			"usable_lanes_at_settlement": 1, "cause": "completed",
			"content_decision": map[string]any{"kind": "capacity_busy", "capacity_decision_id": decisionID},
		}),
		v3CapacityTraceRecord(2, "share", "sender_capacity", runtimeRunID, map[string]any{
			"stage": "admission_denied", "decision_id": decisionID, "revision_id": revisionID,
			"process": capacityScope, "share": capacityScope, "session": capacityScope,
		}),
		v3CapacityTraceRecord(3, "share", "sender_revision", runtimeRunID, map[string]any{
			"stage": "lease_settlement", "cause": "relinquished", "revision_id": revisionID, "lease_id": leaseID,
		}),
	}
	for _, record := range sender {
		record["correlation"] = map[string]any{"protocol_session_id": protocolSessionID}
	}
	sender[0]["correlation"] = map[string]any{
		"protocol_session_id": protocolSessionID, "protocol_operation_id": protocolOperationID,
	}
	v3ReadTraceVectors(t, "share", sender)
}

func v3CapacityTraceRecord(sequence int, command, event, runtimeRunID string, payload map[string]any) map[string]any {
	return map[string]any{
		"schema_version": v3TraceSchemaVersion,
		"sequence":       strconv.Itoa(sequence),
		"time":           "2026-08-23T00:00:00Z",
		"elapsed_ms":     strconv.Itoa(sequence),
		"level":          "debug",
		"event":          event,
		"command":        command,
		"runtime_run_id": runtimeRunID,
		"payload":        payload,
	}
}

func v3CapacityProgressVector() map[string]any {
	return map[string]any{
		"discovery": "complete", "counters_exact": true,
		"discovered_files": "2", "discovered_bytes": "8192",
		"published_files": "1", "published_bytes": "4096",
		"verified_bytes": "4096", "newly_verified_bytes": "4096",
		"file_outcomes": map[string]any{
			"downloaded_files": "1", "resumed_files": "0", "paused_files": "0",
			"collision_files": "0", "item_blocked_files": "0", "failed_files": "0",
			"modified_time_warnings": "0",
		},
		"capacity_wait": map[string]any{
			"active_waiters": "1", "accumulated_wait_ms": "250", "attempts": "2",
		},
	}
}

func v3CapacityScopeVector() map[string]any {
	return map[string]any{
		"stable_handles": "2", "active_leases": "1", "stable_handle_limit": "256", "active_lease_limit": "64",
		"reclaimable_stable_handles": "1", "quarantined_stable_handles": "0",
		"pending_admissions": "1", "active_reclaims": "0",
	}
}

func v3ReadTraceVectors(t *testing.T, command string, records []map[string]any) {
	t.Helper()
	var encoded bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		encoded.Write(line)
		encoded.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), command+"-capacity-trace.ndjson")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	validated, _ := readV3UserTrace(t, path, command)
	if len(validated) != len(records) {
		t.Fatalf("validated capacity records=%d want=%d", len(validated), len(records))
	}
}

func v3CapacityBase64ID(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, v3IdentityBytes))
}

func v3CapacityHexID(fill byte, size int) string {
	return hex.EncodeToString(bytes.Repeat([]byte{fill}, size))
}

func validateV3TraceHex(t *testing.T, value, context string, wantBytes int) {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != wantBytes || hex.EncodeToString(decoded) != value {
		t.Fatalf("user trace field %s is not canonical lowercase hex: %q", context, value)
	}
}
