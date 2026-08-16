package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	v2TraceStringFields = v2TraceFieldSet(`
		time level event command run_id
		receive_operation_id protocol_session_id protocol_operation_id transfer_job_id peer_path_id peer_attempt_id
		protocol_role protocol_operation_stage protocol_request_kind protocol_response_kind protocol_send_outcome
		protocol_response_count protocol_deadline_remaining_ms protocol_operation_elapsed_ms
		protocol_usable_lanes_at_selection protocol_usable_lanes_at_settlement protocol_operation_cause
		transport from_transport to_transport content_path relay_scheme relay_host
		subject_kind file_bytes selected_items state
		failure_code message_key fault_domain fault_scope retry_after_ms result_status drift
		discovery discovered_files discovered_bytes published_files published_bytes verified_bytes newly_verified_bytes
		downloaded_files resumed_files paused_files collision_files item_blocked_files failed_files modified_time_warnings
		directory_failures omitted_diagnostics trace_incomplete_cause lifecycle_dropped progress_dropped events_written
		relay_link_id relay_send_operation_id stage disposition retirement_source cause drain_cause
		webrtc_channel_id webrtc_send_operation_id operation transition terminal_state dropped attempt_sequence attempt_elapsed_ms failure_scope
		file_selection file_settlement tree_settlement node_claims directory_claims file_claims active_file_claims reserved_file_slots
		directory_metadata_bytes checkpoint_records transport_disposition outcome decision active_scans scan_work entries memory_bytes spill_bytes
		legacy_roots_removed root_prefetch_attempt root_prefetch_entry_count root_prefetch_omitted_count
	`)
	v2TraceNumberFields = v2TraceFieldSet(`
		schema_version sequence elapsed_ms lane_id lane_epoch relay_port attempt fault_code exit_code result_elapsed_ms
		candidates_local_emitted candidates_remote_accepted
	`)
	v2TraceBoolFields = v2TraceFieldSet(`
		destination_adjusted stopped_cleanly counters_exact trace_incomplete writer_failed flush_failed schema_limited
		protocol_has_send protocol_send_settled protocol_send_admitted
		terminal settled
	`)
	v2TraceDecimalStringFields = v2TraceFieldSet(`
		file_bytes selected_items retry_after_ms discovered_files discovered_bytes published_files published_bytes verified_bytes newly_verified_bytes
		downloaded_files resumed_files paused_files collision_files item_blocked_files failed_files modified_time_warnings directory_failures omitted_diagnostics
		lifecycle_dropped progress_dropped events_written relay_link_id relay_send_operation_id webrtc_channel_id webrtc_send_operation_id dropped
		attempt_sequence attempt_elapsed_ms node_claims directory_claims file_claims active_file_claims reserved_file_slots directory_metadata_bytes
		checkpoint_records active_scans scan_work entries memory_bytes spill_bytes legacy_roots_removed root_prefetch_attempt
		root_prefetch_entry_count root_prefetch_omitted_count
		protocol_response_count protocol_deadline_remaining_ms protocol_operation_elapsed_ms
		protocol_usable_lanes_at_selection protocol_usable_lanes_at_settlement
	`)
	v2TraceRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func v2TraceFieldSet(value string) map[string]struct{} {
	fields := make(map[string]struct{})
	for field := range strings.FieldsSeq(value) {
		fields[field] = struct{}{}
	}
	return fields
}

func (process *v2Process) forbidUserTrace(values ...string) {
	if process == nil {
		return
	}
	for _, value := range values {
		if value != "" {
			process.traceForbidden = append(process.traceForbidden, value)
		}
	}
}

func (process *v2Process) forbidStderr(values ...string) {
	if process == nil {
		return
	}
	for _, value := range values {
		if value != "" {
			process.stderrForbidden = append(process.stderrForbidden, value)
		}
	}
}

func v2CapabilityForbiddenValues(capability string) []string {
	values := []string{capability}
	if _, fragment, present := strings.Cut(capability, "#"); present && fragment != "" {
		values = append(values, fragment)
	}
	return values
}

func (process *v2Process) validateUserTrace(t *testing.T) {
	t.Helper()
	if process == nil || process.userTracePath == "" || process.userTraceValidated {
		return
	}
	process.userTraceValidated = true
	stderr := process.stderr.String()
	if strings.Contains(stderr, "\x1b[") || strings.Contains(stderr, "\r") {
		t.Fatalf("redirected %s stderr contains ANSI or dynamic refresh: %q", process.component, stderr)
	}
	for _, forbidden := range process.stderrForbidden {
		if strings.Contains(stderr, forbidden) {
			t.Fatalf("redirected %s stderr contains forbidden value %q", process.component, forbidden)
		}
	}
	records, encoded := readV2UserTrace(t, process.userTracePath, process.userTraceCommand)
	for _, forbidden := range process.traceForbidden {
		if v2TraceContainsForbidden(encoded, forbidden) {
			t.Fatalf("%s user trace contains forbidden value %q", process.component, forbidden)
		}
	}
	if len(records) < 2 {
		t.Fatalf("%s user trace has %d records, want lifecycle plus summary", process.component, len(records))
	}
	last := records[len(records)-1]
	if v2TraceString(t, last, "event") != "trace_summary" {
		t.Fatalf("%s user trace has no terminal summary", process.component)
	}
	incomplete := v2TraceBool(t, last, "trace_incomplete")
	hasWarning := strings.Contains(stderr, "Trace is incomplete")
	if incomplete != hasWarning {
		t.Fatalf(
			"%s user trace incomplete=%t but redirected warning=%t",
			process.component,
			incomplete,
			hasWarning,
		)
	}
}

func readV2UserTrace(t *testing.T, path, command string) ([]map[string]json.RawMessage, []byte) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read user trace %q: %v", path, err)
	}
	if len(bytes.TrimSpace(encoded)) == 0 {
		t.Fatalf("user trace %q is empty", path)
	}
	lines := bytes.Split(bytes.TrimSpace(encoded), []byte{'\n'})
	records := make([]map[string]json.RawMessage, 0, len(lines))
	sequences := make(map[uint64]struct{}, len(lines))
	for index, line := range lines {
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var record map[string]json.RawMessage
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode user trace line %d: %v", index+1, err)
		}
		if err := ensureV2TraceEOF(decoder); err != nil {
			t.Fatalf("decode user trace line %d: %v", index+1, err)
		}
		validateV2TraceRecord(t, record, command, sequences)
		records = append(records, record)
	}
	return records, encoded
}

func ensureV2TraceEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateV2TraceRecord(
	t *testing.T,
	record map[string]json.RawMessage,
	command string,
	sequences map[uint64]struct{},
) {
	t.Helper()
	for field, raw := range record {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			t.Fatalf("user trace field %q is null", field)
		}
		switch {
		case v2TraceHas(v2TraceStringFields, field):
			value := v2TraceString(t, record, field)
			if v2TraceHas(v2TraceDecimalStringFields, field) {
				if _, err := strconv.ParseUint(value, 10, 64); err != nil {
					t.Fatalf("user trace field %q is not an unsigned decimal string: %q", field, value)
				}
			}
		case v2TraceHas(v2TraceNumberFields, field):
			var value json.Number
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("user trace field %q is not numeric: %v", field, err)
			}
			parsed, err := strconv.ParseInt(value.String(), 10, 64)
			if err != nil || parsed < 0 {
				t.Fatalf("user trace field %q is not an integer: %q", field, value)
			}
		case v2TraceHas(v2TraceBoolFields, field):
			_ = v2TraceBool(t, record, field)
		default:
			t.Fatalf("user trace contains unknown field %q", field)
		}
	}
	if v2TraceInt64(t, record, "schema_version") != 1 {
		t.Fatal("user trace schema_version is not 1")
	}
	sequence := uint64(v2TraceInt64(t, record, "sequence"))
	if sequence == 0 || sequence > 9_007_199_254_740_991 {
		t.Fatalf("user trace sequence is outside the JSON-safe range: %d", sequence)
	}
	if _, duplicate := sequences[sequence]; duplicate {
		t.Fatalf("user trace sequence %d is duplicated", sequence)
	}
	sequences[sequence] = struct{}{}
	if elapsed := v2TraceInt64(t, record, "elapsed_ms"); elapsed < 0 {
		t.Fatalf("user trace elapsed_ms is negative: %d", elapsed)
	}
	if _, err := time.Parse(time.RFC3339Nano, v2TraceString(t, record, "time")); err != nil {
		t.Fatalf("user trace time is invalid: %v", err)
	}
	if got := v2TraceString(t, record, "command"); got != command {
		t.Fatalf("user trace command=%q want=%q", got, command)
	}
	if runID := v2TraceString(t, record, "run_id"); !v2TraceRunID.MatchString(runID) {
		t.Fatalf("user trace run_id is not canonical: %q", runID)
	}
	if event := v2TraceString(t, record, "event"); event == "" {
		t.Fatal("user trace event is empty")
	}
	switch v2TraceString(t, record, "level") {
	case "debug", "info", "warning", "error":
	default:
		t.Fatal("user trace level is outside the closed vocabulary")
	}
}

func v2TraceContainsForbidden(encoded []byte, forbidden string) bool {
	if forbidden == "" {
		return false
	}
	candidates := [][]byte{[]byte(forbidden)}
	if escaped, err := json.Marshal(forbidden); err == nil && len(escaped) >= 2 {
		candidates = append(candidates, escaped[1:len(escaped)-1])
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(forbidden)))
	candidates = append(candidates, []byte(digest), []byte(digest[:16]))
	if len(forbidden) > 16 {
		candidates = append(candidates, []byte(forbidden[:16]))
	}
	for _, candidate := range candidates {
		if len(candidate) > 0 && bytes.Contains(encoded, candidate) {
			return true
		}
	}
	return false
}

func v2TraceHas(fields map[string]struct{}, field string) bool {
	_, present := fields[field]
	return present
}

func v2TraceString(t *testing.T, record map[string]json.RawMessage, field string) string {
	t.Helper()
	raw, present := record[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %q is not a string: %v", field, err)
	}
	return value
}

func v2TraceBool(t *testing.T, record map[string]json.RawMessage, field string) bool {
	t.Helper()
	raw, present := record[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %q is not a bool: %v", field, err)
	}
	return value
}

func v2TraceInt64(t *testing.T, record map[string]json.RawMessage, field string) int64 {
	t.Helper()
	raw, present := record[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %q is not numeric: %v", field, err)
	}
	parsed, err := strconv.ParseInt(value.String(), 10, 64)
	if err != nil {
		t.Fatalf("user trace field %q is not an integer: %v", field, err)
	}
	return parsed
}

func requireV2UserTraceFact(t *testing.T, process *v2Process, event, field, value string) {
	t.Helper()
	records, _ := readV2UserTrace(t, process.userTracePath, process.userTraceCommand)
	for _, record := range records {
		if v2TraceString(t, record, "event") != event {
			continue
		}
		if raw, present := record[field]; present {
			var got string
			if json.Unmarshal(raw, &got) == nil && got == value {
				return
			}
		}
	}
	t.Fatalf("%s user trace has no %s with %s=%q", process.component, event, field, value)
}
