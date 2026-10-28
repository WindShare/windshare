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

func TestUserTraceCapacityWaitRemainsProductState(t *testing.T) {
	v5ReadTraceVectors(t, "get", []map[string]any{v5CapacityTraceRecord(1, "get", "transfer_progress", v5CapacityBase64ID(1), map[string]any{
		"receive_operation_id": v5CapacityBase64ID(2), "transfer_job_id": v5CapacityBase64ID(3), "progress": v5CapacityProgressVector(),
	})})
}

func v5CapacityTraceRecord(sequence int, command, event, runtimeRunID string, payload map[string]any) map[string]any {
	return map[string]any{
		"schema_version": v5TraceSchemaVersion,
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

func v5CapacityProgressVector() map[string]any {
	return map[string]any{
		"discovery": "complete", "counters_exact": true,
		"discovered_files": "2", "discovered_bytes": "8192",
		"published_files": "1", "published_bytes": "4096",
		"verified_bytes": "4096", "newly_verified_bytes": "4096",
		"previously_published_bytes": "0", "restart_reusable_bytes": "0",
		"file_outcomes": map[string]any{
			"downloaded_files": "1", "resumed_files": "0", "paused_files": "0",
			"previously_published_files": "0",
			"collision_files":            "0", "item_blocked_files": "0", "failed_files": "0",
			"modified_time_warnings": "0",
		},
		"capacity_wait": map[string]any{
			"active_waiters": "1", "accumulated_wait_ms": "250", "attempts": "2",
		},
	}
}

func v5ReadTraceVectors(t *testing.T, command string, records []map[string]any) {
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
	validated, _ := readV5UserTrace(t, path, command)
	if len(validated) != len(records) {
		t.Fatalf("validated capacity records=%d want=%d", len(validated), len(records))
	}
}

func v5CapacityBase64ID(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, v5IdentityBytes))
}

func validateV5TraceHex(t *testing.T, value, context string, wantBytes int) {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != wantBytes || hex.EncodeToString(decoded) != value {
		t.Fatalf("user trace field %s is not canonical lowercase hex: %q", context, value)
	}
}
