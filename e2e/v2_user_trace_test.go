package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		relay_link_id relay_session_id relay_send_operation_id stage disposition retirement_source cause drain_cause relay_dropped
		webrtc_channel_id webrtc_send_operation_id operation transition terminal_state dropped attempt_sequence attempt_elapsed_ms failure_scope
		peer_offer_operation_id peer_grant_operation_id peer_phase peer_deadline_ms peer_admission_disposition peer_response_delivery
		peer_lane_rejection_code peer_rejection_retry_after_ms peer_failed_at_stage
		file_selection file_settlement tree_settlement node_claims directory_claims file_claims active_file_claims reserved_file_slots
		directory_metadata_bytes checkpoint_records transport_disposition outcome decision active_scans scan_work entries memory_bytes spill_bytes
		legacy_roots_removed root_prefetch_attempt root_prefetch_entry_count root_prefetch_omitted_count
		receive_intent_digest output_session_id filesystem_certification filesystem_root_disposition
		filesystem_native_lock_scope filesystem_native_lock_milestone filesystem_runtime_component filesystem_runtime_operation
		filesystem_runtime_decision filesystem_checkpoint_decision filesystem_operation_id filesystem_claim_id filesystem_failure_stage
		filesystem_reconciliation_step filesystem_native_error_class
		lane_route delivered_blocks delivered_bytes failed_block_attempts reassigned_blocks
		observer_loss_category observer_loss_reason observer_loss_count
		receiver_local_generation receiver_transition_authority receiver_disposition receiver_transition_provenance
		receiver_consequence_provenance receiver_local_stop_reason
	`)
	v2TraceNumberFields = v2TraceFieldSet(`
		schema_version sequence elapsed_ms lane_id lane_epoch relay_port attempt fault_code exit_code result_elapsed_ms
		candidates_local_emitted candidates_remote_accepted
	`)
	v2TraceBoolFields = v2TraceFieldSet(`
		destination_adjusted stopped_cleanly counters_exact trace_incomplete writer_failed flush_failed schema_limited
		protocol_has_send protocol_send_settled protocol_send_admitted
		terminal settled incomplete receiver_diagnostics_truncated receiver_peer_shutdown_failed receiver_channel_drain_failed
	`)
	v2TraceStringSliceFields = v2TraceFieldSet(`
		receiver_benign_components receiver_retained_cause_classes receiver_teardown_transitions
	`)
	v2TraceFilesystemCheckpointDecisions = v2TraceFieldSet(`
		absent exact revision_conflict ownership_conflict invalid
	`)
	v2TraceDecimalStringFields = v2TraceFieldSet(`
		file_bytes selected_items retry_after_ms discovered_files discovered_bytes published_files published_bytes verified_bytes newly_verified_bytes
		downloaded_files resumed_files paused_files collision_files item_blocked_files failed_files modified_time_warnings directory_failures omitted_diagnostics
		lifecycle_dropped progress_dropped events_written relay_link_id relay_send_operation_id webrtc_channel_id webrtc_send_operation_id dropped
		attempt_sequence attempt_elapsed_ms node_claims directory_claims file_claims active_file_claims reserved_file_slots directory_metadata_bytes
		checkpoint_records active_scans scan_work entries memory_bytes spill_bytes legacy_roots_removed root_prefetch_attempt
		root_prefetch_entry_count root_prefetch_omitted_count relay_dropped delivered_blocks delivered_bytes failed_block_attempts
		reassigned_blocks observer_loss_count receiver_local_generation
		peer_deadline_ms peer_rejection_retry_after_ms
		filesystem_operation_id filesystem_claim_id
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
	warningCount := strings.Count(stderr, "Trace is incomplete")
	if warningCount > 1 {
		t.Fatalf("%s emitted %d trace-incomplete warnings, want at most one", process.component, warningCount)
	}
	if incomplete != (warningCount == 1) {
		t.Fatalf(
			"%s user trace incomplete=%t but redirected warning count=%d",
			process.component,
			incomplete,
			warningCount,
		)
	}
	assertV2ProducerFactsPrecedeCommandResult(t, process.component, records)
}

func assertV2ProducerFactsPrecedeCommandResult(t *testing.T, component string, records []map[string]json.RawMessage) {
	t.Helper()
	terminal := -1
	for index, record := range records {
		switch v2TraceString(t, record, "event") {
		case "transfer_settled", "sharing_stopped", "command_failed":
			if terminal < 0 {
				terminal = index
			}
		}
	}
	if terminal < 0 {
		t.Fatalf("%s user trace omitted its command result", component)
	}
	for index := terminal + 1; index < len(records); index++ {
		event := v2TraceString(t, records[index], "event")
		switch event {
		case "relay_lifecycle", "webrtc_lifecycle", "peer_attempt", "receiver_termination", "lane_settlement", "observer_loss":
			t.Fatalf("%s user trace emitted %s after its command result", component, event)
		}
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
		case v2TraceHas(v2TraceStringSliceFields, field):
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil || values == nil {
				t.Fatalf("user trace field %q is not a string array: %v", field, err)
			}
		default:
			t.Fatalf("user trace contains unknown field %q", field)
		}
	}
	if v2TraceInt64(t, record, "schema_version") != 2 {
		t.Fatal("user trace schema_version is not 2")
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
	validateV2DiagnosticRecord(t, record)
}

func validateV2DiagnosticRecord(t *testing.T, record map[string]json.RawMessage) {
	t.Helper()
	event := v2TraceString(t, record, "event")
	if _, present := record["filesystem_checkpoint_decision"]; present {
		decision := v2TraceString(t, record, "filesystem_checkpoint_decision")
		if !v2ValidFilesystemCheckpointDecision(event, decision) {
			t.Fatalf("filesystem checkpoint decision %q is outside the closed vocabulary for event %q", decision, event)
		}
	}
	switch event {
	case "lane_settlement":
		route := v2TraceString(t, record, "lane_route")
		if route != "relay" && route != "direct" {
			t.Fatalf("lane settlement has unknown route %q", route)
		}
		if session := v2TraceString(t, record, "protocol_session_id"); !v2TraceRunID.MatchString(session) {
			t.Fatalf("lane settlement has invalid protocol session %q", session)
		}
		_ = v2TraceInt64(t, record, "lane_id")
		_ = v2TraceInt64(t, record, "lane_epoch")
		for _, field := range []string{"delivered_blocks", "delivered_bytes", "failed_block_attempts", "reassigned_blocks"} {
			_ = v2TraceDecimal(t, record, field)
		}
		_ = v2TraceBool(t, record, "incomplete")
	case "observer_loss":
		if !v2KnownObserverLossCategory(v2TraceString(t, record, "observer_loss_category")) {
			t.Fatal("observer loss category is outside the closed vocabulary")
		}
		if !v2KnownObserverLossReason(v2TraceString(t, record, "observer_loss_reason")) {
			t.Fatal("observer loss reason is outside the closed vocabulary")
		}
		if v2TraceDecimal(t, record, "observer_loss_count") == 0 {
			t.Fatal("observer loss count is zero")
		}
	case "receiver_termination":
		if reason := v2TraceString(t, record, "receiver_local_stop_reason"); reason != "none" && reason != "caller_stop" && reason != "output_admission_stop" &&
			reason != "runtime_session_failure" && reason != "normal_completion" {
			t.Fatalf("receiver termination has unknown local stop reason %q", reason)
		}
	case "relay_lifecycle":
		if v2TraceString(t, record, "stage") == "send_admitted" {
			t.Fatal("ordinary successful relay sends must not enter the user trace")
		}
	}
}

func v2ValidFilesystemCheckpointDecision(event, decision string) bool {
	return event == "filesystem_output" && v2TraceHas(v2TraceFilesystemCheckpointDecisions, decision)
}

func v2KnownObserverLossCategory(value string) bool {
	switch value {
	case "relay_lifecycle", "webrtc_lifecycle", "sender_attempt", "receiver_termination", "lane_settlement",
		"protocol_operation", "transfer_lifecycle", "filesystem_output", "catalog_storage", "root_prefetch", "command_adapter":
		return true
	default:
		return false
	}
}

func v2KnownObserverLossReason(value string) bool {
	switch value {
	case "unknown_enum", "invalid_identity", "invalid_stage_field_combination", "event_contract_rejection",
		"adapter_capacity_timeout", "trace_queue", "recorder_closed":
		return true
	default:
		return false
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

func v2TraceDecimal(t *testing.T, record map[string]json.RawMessage, field string) uint64 {
	t.Helper()
	value := v2TraceString(t, record, field)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("user trace field %q is not an unsigned decimal string: %q", field, value)
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

func assertV2UserTraceProductDiagnostics(t *testing.T, process *v2Process, receiveOperationID string) {
	t.Helper()
	records, _ := readV2UserTrace(t, process.userTracePath, process.userTraceCommand)
	checkpointReconciled := false
	receiverCompleted := false
	laneSettlements := 0
	laneAdoptions := 0
	laneIdentities := make(map[string]struct{})
	for _, record := range records {
		event := v2TraceString(t, record, "event")
		switch event {
		case "filesystem_output":
			if raw, ok := record["operation"]; ok {
				var operation string
				if json.Unmarshal(raw, &operation) == nil && operation == "checkpoint_reconciled" {
					if v2TraceString(t, record, "receive_operation_id") != receiveOperationID {
						t.Fatal("checkpoint reconciliation belongs to a different retained operation")
					}
					checkpointReconciled = true
				}
			}
		case "lane_settlement":
			laneSettlements++
			identity := fmt.Sprintf(
				"%s/%d/%d",
				v2TraceString(t, record, "protocol_session_id"),
				v2TraceInt64(t, record, "lane_id"),
				v2TraceInt64(t, record, "lane_epoch"),
			)
			if _, duplicate := laneIdentities[identity]; duplicate {
				t.Fatalf("lane settlement %q was emitted more than once", identity)
			}
			laneIdentities[identity] = struct{}{}
		case "lane_adopted":
			laneAdoptions++
		case "receiver_termination":
			if v2TraceString(t, record, "receiver_local_stop_reason") == "normal_completion" {
				receiverCompleted = true
			}
		case "fallback":
			t.Fatal("successful retained-operation recovery emitted a fallback record")
		case "observer_loss":
			t.Fatal("successful retained-operation recovery lost diagnostic observations")
		}
	}
	if !checkpointReconciled {
		t.Fatal("user trace omitted retained-operation checkpoint reconciliation")
	}
	// The initial relay lane exists before lane-adoption observations; every
	// additional incarnation must have a corresponding adoption record.
	if laneSettlements == 0 || laneSettlements > laneAdoptions+1 {
		t.Fatalf("user trace lane settlements=%d adoptions=%d, want one bounded summary per incarnation", laneSettlements, laneAdoptions)
	}
	if !receiverCompleted {
		t.Fatal("user trace omitted normal receiver termination")
	}
}

func assertV2UserTraceTransportDiagnostics(
	t *testing.T,
	process *v2Process,
	wantRoute string,
	requireReceiverTermination bool,
) {
	t.Helper()
	records, _ := readV2UserTrace(t, process.userTracePath, process.userTraceCommand)
	delivered := false
	receiverCompleted := false
	for _, record := range records {
		switch v2TraceString(t, record, "event") {
		case "lane_settlement":
			if v2TraceString(t, record, "lane_route") == wantRoute &&
				v2TraceDecimal(t, record, "delivered_blocks") > 0 &&
				v2TraceDecimal(t, record, "delivered_bytes") > 0 {
				delivered = true
			}
		case "receiver_termination":
			if v2TraceString(t, record, "receiver_local_stop_reason") == "normal_completion" {
				receiverCompleted = true
			}
		case "fallback":
			t.Fatalf("%s-only transfer emitted an unexpected fallback record", wantRoute)
		case "observer_loss":
			t.Fatalf("%s-only transfer lost diagnostic observations", wantRoute)
		}
	}
	if !delivered {
		t.Fatalf("user trace omitted authenticated %s lane delivery", wantRoute)
	}
	if requireReceiverTermination && !receiverCompleted {
		t.Fatal("direct transfer trace omitted normal receiver termination")
	}
}

func TestUserTraceV2FilesystemCheckpointDecisionVocabulary(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		decision string
		want     bool
	}{
		{"absent", "filesystem_output", "absent", true},
		{"exact", "filesystem_output", "exact", true},
		{"revision conflict", "filesystem_output", "revision_conflict", true},
		{"ownership conflict", "filesystem_output", "ownership_conflict", true},
		{"invalid", "filesystem_output", "invalid", true},
		{"empty", "filesystem_output", "", false},
		{"case variant", "filesystem_output", "RevisionConflict", false},
		{"display spelling", "filesystem_output", "revision-conflict", false},
		{"sensitive payload", "filesystem_output", `checkpoint/path/owned-object-id`, false},
		{"wrong event", "transfer_lifecycle", "exact", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := v2ValidFilesystemCheckpointDecision(test.event, test.decision); got != test.want {
				t.Fatalf("valid=%t want=%t", got, test.want)
			}
		})
	}
}

func TestUserTraceV2DiagnosticContract(t *testing.T) {
	runID := strings.Repeat("1", 32)
	base := func(sequence int, event string) map[string]any {
		return map[string]any{
			"schema_version": 2,
			"sequence":       sequence,
			"time":           "2026-08-17T00:00:00Z",
			"elapsed_ms":     sequence,
			"level":          "debug",
			"event":          event,
			"command":        "get",
			"run_id":         runID,
		}
	}
	lane := base(1, "lane_settlement")
	lane["protocol_session_id"] = runID
	lane["lane_id"] = 1
	lane["lane_epoch"] = 0
	lane["lane_route"] = "relay"
	lane["delivered_blocks"] = "2"
	lane["delivered_bytes"] = "8192"
	lane["failed_block_attempts"] = "0"
	lane["reassigned_blocks"] = "0"
	lane["incomplete"] = false
	loss := base(2, "observer_loss")
	loss["observer_loss_category"] = "filesystem_output"
	loss["observer_loss_reason"] = "trace_queue"
	loss["observer_loss_count"] = "1"
	termination := base(3, "receiver_termination")
	termination["receiver_local_generation"] = "1"
	termination["receiver_transition_authority"] = "local"
	termination["receiver_disposition"] = "session_unavailable"
	termination["receiver_transition_provenance"] = "local_explicit_stop"
	termination["receiver_consequence_provenance"] = "local_explicit_stop"
	termination["receiver_local_stop_reason"] = "output_admission_stop"
	termination["receiver_diagnostics_truncated"] = false
	termination["receiver_benign_components"] = []string{}
	termination["receiver_retained_cause_classes"] = []string{}
	termination["receiver_teardown_transitions"] = []string{}
	termination["receiver_peer_shutdown_failed"] = false
	termination["receiver_channel_drain_failed"] = false
	adaptive := base(4, "content_path_selected")
	adaptive["content_path"] = "direct_and_relay"
	recordVectors := []map[string]any{lane, loss, termination, adaptive}
	for index, decision := range []string{"absent", "exact", "revision_conflict", "ownership_conflict", "invalid"} {
		filesystem := base(index+5, "filesystem_output")
		filesystem["operation"] = "runtime_decision"
		filesystem["filesystem_checkpoint_decision"] = decision
		recordVectors = append(recordVectors, filesystem)
	}

	var encoded bytes.Buffer
	for _, record := range recordVectors {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		encoded.Write(line)
		encoded.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "trace.ndjson")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	records, _ := readV2UserTrace(t, path, "get")
	if len(records) != len(recordVectors) {
		t.Fatalf("diagnostic records=%d want=%d", len(records), len(recordVectors))
	}
	for _, record := range records {
		if v2TraceString(t, record, "event") == "fallback" {
			t.Fatal("adaptive relay admission or output-admission shutdown produced fallback evidence")
		}
	}
}
