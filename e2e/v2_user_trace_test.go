package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	records, encoded := readV5UserTrace(t, process.userTracePath, process.userTraceCommand)
	for _, forbidden := range process.traceForbidden {
		if v5TraceContainsForbidden(encoded, forbidden) {
			t.Fatalf("%s user trace contains forbidden value %q", process.component, forbidden)
		}
	}
	if len(records) < 2 {
		t.Fatalf("%s user trace has %d records, want lifecycle plus summary", process.component, len(records))
	}
	last := records[len(records)-1]
	if last.Event != "trace_summary" {
		t.Fatalf("%s user trace has no terminal summary", process.component)
	}
	incomplete := v5TraceBoolField(t, last.Payload, "incomplete")
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
	assertV5ProducerFactsPrecedeCommandResult(t, process.component, records)
	registerCriticalV5ProtocolCorrelation(t, process, records)
}

func assertV5ProducerFactsPrecedeCommandResult(t *testing.T, component string, records []v5TraceRecord) {
	t.Helper()
	terminal := -1
	for index, record := range records {
		switch record.Event {
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
		switch event := records[index].Event; event {
		case "engine_task_observed", "relay_lifecycle", "webrtc_lifecycle", "peer_attempt", "receiver_termination", "lane_settlement", "observer_loss":
			t.Fatalf("%s user trace emitted %s after its command result", component, event)
		}
	}
}

func readV5UserTrace(t *testing.T, path, command string) ([]v5TraceRecord, []byte) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read user trace %q: %v", path, err)
	}
	if len(bytes.TrimSpace(encoded)) == 0 {
		t.Fatalf("user trace %q is empty", path)
	}
	lines := bytes.Split(bytes.TrimSpace(encoded), []byte{'\n'})
	records := make([]v5TraceRecord, 0, len(lines))
	sequences := make(map[uint64]struct{}, len(lines))
	runtimeRunID := ""
	for index, line := range lines {
		record := validateV5TraceRecord(t, line, command, sequences)
		if runtimeRunID == "" {
			runtimeRunID = record.RuntimeRunID
		} else if runtimeRunID != record.RuntimeRunID {
			t.Fatalf("user trace line %d changed local runtime_run_id", index+1)
		}
		records = append(records, record)
	}
	return records, encoded
}

func validateV5TraceRecord(
	t *testing.T,
	line []byte,
	command string,
	sequences map[uint64]struct{},
) v5TraceRecord {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var envelope map[string]json.RawMessage
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode user trace record: %v", err)
	}
	if err := ensureV5TraceEOF(decoder); err != nil {
		t.Fatalf("decode user trace record: %v", err)
	}
	allowedEnvelope := map[string]struct{}{
		"schema_version": {}, "sequence": {}, "time": {}, "elapsed_ms": {}, "level": {},
		"event": {}, "command": {}, "runtime_run_id": {}, "correlation": {}, "payload": {}, "scope": {}, "source_time": {},
	}
	for field, raw := range envelope {
		if _, allowed := allowedEnvelope[field]; !allowed {
			t.Fatalf("user trace contains unknown envelope field %q", field)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			t.Fatalf("user trace envelope field %q is null", field)
		}
	}
	for _, field := range []string{
		"schema_version", "sequence", "time", "elapsed_ms", "level", "event", "command", "runtime_run_id", "payload",
	} {
		if _, present := envelope[field]; !present {
			t.Fatalf("user trace is missing envelope field %q", field)
		}
	}
	if version := v5TraceIntegerField(t, envelope, "schema_version"); version != v5TraceSchemaVersion {
		t.Fatalf("user trace schema_version=%d want=%d", version, v5TraceSchemaVersion)
	}
	sequence := v5TraceDecimalField(t, envelope, "sequence")
	if sequence == 0 {
		t.Fatal("user trace sequence is zero")
	}
	if _, duplicate := sequences[sequence]; duplicate {
		t.Fatalf("user trace sequence %d is duplicated", sequence)
	}
	sequences[sequence] = struct{}{}
	_ = v5TraceDecimalField(t, envelope, "elapsed_ms")
	if _, err := time.Parse(time.RFC3339Nano, v5TraceStringField(t, envelope, "time")); err != nil {
		t.Fatalf("user trace time is invalid: %v", err)
	}
	if got := v5TraceStringField(t, envelope, "command"); got != command {
		t.Fatalf("user trace command=%q want=%q", got, command)
	}
	runtimeRunID := v5TraceStringField(t, envelope, "runtime_run_id")
	validateV5TraceIdentity(t, runtimeRunID, "runtime_run_id", v5IdentityBytes)
	event := v5TraceStringField(t, envelope, "event")
	schema, known := v5TracePayloadSchemas[event]
	_, diagnostic := envelope["scope"]
	if !known && !diagnostic {
		t.Fatalf("unknown product event %q", event)
	}
	switch level := v5TraceStringField(t, envelope, "level"); level {
	case "debug", "info", "warn", "error":
	default:
		t.Fatalf("user trace level %q is outside the closed vocabulary", level)
	}
	payload := v5TraceObjectMap(t, envelope["payload"], event+" payload")
	if !diagnostic {
		validateV5TraceObject(t, payload, schema, event+" payload")
	}
	var correlation *v5TraceCorrelation
	if raw, present := envelope["correlation"]; present {
		var projected v5TraceCorrelation
		if diagnostic {
			object := v5TraceObjectMap(t, raw, event+" correlation")
			common := map[string]json.RawMessage{}
			for _, name := range []string{"protocol_session_id", "protocol_operation_id", "peer_path_id", "peer_attempt_id", "lane_id", "lane_epoch"} {
				if v, ok := object[name]; ok {
					common[name] = v
				}
			}
			if len(common) > 0 {
				encoded, _ := json.Marshal(common)
				projected = validateV5TraceCorrelation(t, encoded, event+" correlation")
			}
			for name, target := range map[string]*string{"share_instance_id": &projected.ShareInstanceID, "receive_operation_id": &projected.ReceiveOperationID, "transfer_job_id": &projected.TransferJobID} {
				if raw, ok := object[name]; ok {
					*target = v5TraceStringRaw(t, raw, name)
				}
			}
		} else {
			projected = validateV5TraceCorrelation(t, raw, event+" correlation")
		}
		correlation = &projected
	}
	record := v5TraceRecord{
		Command: command, Event: event, RuntimeRunID: runtimeRunID, Correlation: correlation, Payload: payload,
	}
	if diagnostic {
		if err := json.Unmarshal(envelope["scope"], &record.Scope); err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339Nano, v5TraceStringField(t, envelope, "source_time")); err != nil {
			t.Fatal(err)
		}
	}
	validateV5DiagnosticRecord(t, record)
	return record
}

func ensureV5TraceEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateV5TraceObject(
	t *testing.T,
	object map[string]json.RawMessage,
	schema *v5TraceObjectSchema,
	context string,
) {
	t.Helper()
	for name, raw := range object {
		field, known := schema.fields[name]
		if !known {
			t.Fatalf("%s contains unknown field %q", context, name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if field.nullable {
				continue
			}
			t.Fatalf("%s field %q is null", context, name)
		}
		validateV5TraceValue(t, raw, field, context)
	}
	for name, field := range schema.fields {
		if _, present := object[name]; !present && !field.optional {
			t.Fatalf("%s is missing field %q", context, name)
		}
	}
}

func validateV5TraceValue(t *testing.T, raw json.RawMessage, field v5TraceFieldSchema, context string) {
	t.Helper()
	fieldContext := context + "." + field.name
	switch field.kind {
	case v5TraceString:
		if value := v5TraceStringRaw(t, raw, fieldContext); value == "" {
			t.Fatalf("%s is empty", fieldContext)
		}
	case v5TraceIdentity:
		validateV5TraceIdentity(t, v5TraceStringRaw(t, raw, fieldContext), fieldContext, v5IdentityBytes)
	case v5TraceRelaySessionIdentity:
		validateV5TraceIdentity(t, v5TraceStringRaw(t, raw, fieldContext), fieldContext, 8)
	case v5TraceDecimal:
		_ = v5TraceDecimalRaw(t, raw, fieldContext)
	case v5TraceInteger:
		_ = v5TraceIntegerRaw(t, raw, fieldContext)
	case v5TraceFraction:
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > 1 {
			t.Fatalf("%s is not a fraction in [0,1]: %s", fieldContext, raw)
		}
	case v5TraceRate:
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
			t.Fatalf("%s is not a nonnegative finite rate: %s", fieldContext, raw)
		}
	case v5TraceBool:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s is not a bool: %v", fieldContext, err)
		}
	case v5TraceStringSlice:
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			t.Fatalf("%s is not a string array: %v", fieldContext, err)
		}
		for _, value := range values {
			if value == "" {
				t.Fatalf("%s contains an empty value", fieldContext)
			}
		}
	case v5TraceObject:
		object := v5TraceObjectMap(t, raw, fieldContext)
		validateV5TraceObject(t, object, field.object, fieldContext)
	case v5TraceHexIdentity:
		validateV5TraceHex(t, v5TraceStringRaw(t, raw, fieldContext), fieldContext, v5IdentityBytes)
	case v5TraceHexDigest:
		validateV5TraceHex(t, v5TraceStringRaw(t, raw, fieldContext), fieldContext, v5DigestBytes)
	case v5TraceCorrelationValue:
		_ = validateV5TraceCorrelation(t, raw, fieldContext)
	case v5TraceTimestamp:
		value := v5TraceStringRaw(t, raw, fieldContext)
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			t.Fatalf("%s invalid timestamp: %v", fieldContext, err)
		}
	case v5TraceRawString:
		_ = v5TraceStringRaw(t, raw, fieldContext)
	case v5TraceObjectSlice:
		var values []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			t.Fatalf("%s is not an object array: %v", fieldContext, err)
		}
		for i, value := range values {
			validateV5TraceObject(t, value, field.object, fmt.Sprintf("%s[%d]", fieldContext, i))
		}
	default:
		t.Fatalf("%s has unsupported schema kind %d", fieldContext, field.kind)
	}
}

func validateV5TraceCorrelation(t *testing.T, raw json.RawMessage, context string) v5TraceCorrelation {
	t.Helper()
	object := v5TraceObjectMap(t, raw, context)
	allowed := map[string]struct{}{
		"protocol_session_id": {}, "protocol_operation_id": {}, "peer_path_id": {},
		"peer_attempt_id": {}, "lane_id": {}, "lane_epoch": {},
	}
	for field, value := range object {
		if _, known := allowed[field]; !known {
			t.Fatalf("%s contains unknown field %q", context, field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			t.Fatalf("%s field %q is null", context, field)
		}
	}
	if len(object) == 0 {
		t.Fatalf("%s is empty", context)
	}
	correlation := v5TraceCorrelation{}
	for field, target := range map[string]*string{
		"protocol_session_id":   &correlation.ProtocolSessionID,
		"protocol_operation_id": &correlation.ProtocolOperationID,
		"peer_path_id":          &correlation.PeerPathID,
		"peer_attempt_id":       &correlation.PeerAttemptID,
	} {
		if value, present := object[field]; present {
			*target = v5TraceStringRaw(t, value, context+"."+field)
			validateV5TraceIdentity(t, *target, context+"."+field, v5IdentityBytes)
		}
	}
	if correlation.ProtocolOperationID != "" && correlation.ProtocolSessionID == "" {
		t.Fatalf("%s has an operation without a protocol session", context)
	}
	if correlation.PeerAttemptID != "" && correlation.PeerPathID == "" {
		t.Fatalf("%s has a peer attempt without a peer path", context)
	}
	laneIDRaw, hasLaneID := object["lane_id"]
	laneEpochRaw, hasLaneEpoch := object["lane_epoch"]
	if hasLaneID != hasLaneEpoch {
		t.Fatalf("%s must contain lane_id and lane_epoch together", context)
	}
	if hasLaneID {
		laneID := v5TraceIntegerRaw(t, laneIDRaw, context+".lane_id")
		laneEpoch := v5TraceIntegerRaw(t, laneEpochRaw, context+".lane_epoch")
		if laneID > uint64(^uint32(0)) || laneEpoch > uint64(^uint32(0)) {
			t.Fatalf("%s lane identity exceeds uint32", context)
		}
		projectedLaneID := uint32(laneID)
		projectedLaneEpoch := uint32(laneEpoch)
		correlation.LaneID = &projectedLaneID
		correlation.LaneEpoch = &projectedLaneEpoch
	}
	return correlation
}

func validateV5DiagnosticRecord(t *testing.T, record v5TraceRecord) {
	t.Helper()
	switch record.Event {
	case "engine_task_observed":
		validateV5EngineTaskRecord(t, record)
	case "lane_adopted", "lane_settlement":
		if record.Correlation == nil || record.Correlation.ProtocolSessionID == "" ||
			record.Correlation.LaneID == nil || record.Correlation.LaneEpoch == nil {
			t.Fatalf("%s lacks protocol session and lane correlation", record.Event)
		}
	case "protocol_operation", "sender_content_decision", "protocol_response_send_not_started", "protocol_response_send_returned", "protocol_send_attempt_settled", "protocol_error_received":
		if record.Correlation == nil || record.Correlation.ProtocolSessionID == "" ||
			record.Correlation.ProtocolOperationID == "" {
			t.Fatal("protocol operation lacks shared session/operation correlation")
		}
	case "filesystem_output":
		if raw, present := record.Payload["checkpoint_decision"]; present {
			decision := v5TraceStringRaw(t, raw, "filesystem_output.checkpoint_decision")
			if decision != "" && !v5ValidFilesystemCheckpointDecision(record.Event, decision) {
				t.Fatalf(
					"filesystem checkpoint decision %q is outside the closed vocabulary",
					decision,
				)
			}
		}
	case "observer_loss":
		if !v5KnownObserverLossCategory(v5TraceStringField(t, record.Payload, "category")) {
			t.Fatal("observer loss category is outside the closed vocabulary")
		}
		if !v5KnownObserverLossReason(v5TraceStringField(t, record.Payload, "reason")) {
			t.Fatal("observer loss reason is outside the closed vocabulary")
		}
		if v5TraceDecimalField(t, record.Payload, "count") == 0 {
			t.Fatal("observer loss count is zero")
		}
	case "receiver_termination":
		switch reason := v5TraceStringField(t, record.Payload, "local_stop_reason"); reason {
		case "none", "caller", "output_admission", "runtime_session_failure", "normal_completion":
		default:
			t.Fatalf("receiver termination has unknown local stop reason %q", reason)
		}
	case "relay_lifecycle":
		// Terminal admission and failures after admission explain shutdown races.
		// Only ordinary successful admission is intentionally absent from tracing.
		if v5TraceStringField(t, record.Payload, "stage") == "send_admitted" &&
			!v5TraceBoolField(t, record.Payload, "terminal") &&
			v5TraceStringField(t, record.Payload, "cause") == "none" {
			t.Fatal("ordinary successful relay sends must not enter the user trace")
		}
	}
}

func v5ValidFilesystemCheckpointDecision(event, decision string) bool {
	if event != "filesystem_output" {
		return false
	}
	switch decision {
	case "absent", "exact", "revision_conflict", "ownership_conflict", "invalid":
		return true
	default:
		return false
	}
}

func v5KnownObserverLossCategory(value string) bool {
	switch value {
	case "relay_lifecycle", "webrtc_lifecycle", "sender_attempt", "receiver_termination", "lane_settlement",
		"protocol_operation", "transfer_lifecycle", "filesystem_output", "catalog_storage", "root_prefetch",
		"sender_capacity", "sender_revision", "native_connectivity", "command_adapter", "engine_observation":
		return true
	default:
		return false
	}
}

func v5KnownObserverLossReason(value string) bool {
	switch value {
	case "unknown_enum", "invalid_identity", "invalid_stage_field_combination", "event_contract_rejection",
		"adapter_capacity_timeout", "trace_queue", "recorder_closed", "observation_capacity", "consumer_undrained", "signature_capacity", "observer_panic":
		return true
	default:
		return false
	}
}

func v5TraceObjectMap(t *testing.T, raw json.RawMessage, context string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		t.Fatalf("%s is not an object: %v", context, err)
	}
	return object
}

func v5TraceStringField(t *testing.T, object map[string]json.RawMessage, field string) string {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v5TraceStringRaw(t, raw, field)
}

func v5TraceStringRaw(t *testing.T, raw json.RawMessage, context string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %s is not a string: %v", context, err)
	}
	return value
}

func v5TraceBoolField(t *testing.T, object map[string]json.RawMessage, field string) bool {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %q is not a bool: %v", field, err)
	}
	return value
}

func v5TraceIntegerField(t *testing.T, object map[string]json.RawMessage, field string) uint64 {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v5TraceIntegerRaw(t, raw, field)
}

func v5TraceIntegerRaw(t *testing.T, raw json.RawMessage, context string) uint64 {
	t.Helper()
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %s is not numeric: %v", context, err)
	}
	parsed, err := strconv.ParseUint(value.String(), 10, 64)
	if err != nil {
		t.Fatalf("user trace field %s is not an unsigned integer: %q", context, value)
	}
	return parsed
}

func v5TraceDecimalField(t *testing.T, object map[string]json.RawMessage, field string) uint64 {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v5TraceDecimalRaw(t, raw, field)
}

func v5TraceDecimalRaw(t *testing.T, raw json.RawMessage, context string) uint64 {
	t.Helper()
	value := v5TraceStringRaw(t, raw, context)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		t.Fatalf("user trace field %s is not a canonical unsigned decimal string: %q", context, value)
	}
	return parsed
}

func validateV5TraceIdentity(t *testing.T, value, context string, wantBytes int) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != wantBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		t.Fatalf("user trace field %s is not a canonical unpadded base64url identity: %q", context, value)
	}
}

func v5TraceContainsForbidden(encoded []byte, forbidden string) bool {
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

func requireV2UserTraceFact(t *testing.T, process *v2Process, event, field, value string) {
	t.Helper()
	records, _ := readV5UserTrace(t, process.userTracePath, process.userTraceCommand)
	for _, record := range records {
		if record.Event != event {
			continue
		}
		if raw, present := record.Payload[field]; present {
			var got string
			if json.Unmarshal(raw, &got) == nil && got == value {
				return
			}
		}
	}
	t.Fatalf("%s user trace has no %s payload with %s=%q", process.component, event, field, value)
}

func assertV2UserTraceProductDiagnostics(t *testing.T, process *v2Process, receiveOperationID string) {
	t.Helper()
	receiveOperationID = v5TraceIdentityFromHex(t, receiveOperationID)
	records, _ := readV5UserTrace(t, process.userTracePath, process.userTraceCommand)
	checkpointReconciled := false
	receiverCompleted := false
	laneSettlements := 0
	laneAdoptions := 0
	laneIdentities := make(map[string]struct{})
	for _, record := range records {
		switch record.Event {
		case "filesystem_output":
			if operation, ok := record.Payload["operation"]; ok &&
				v5TraceStringRaw(t, operation, "filesystem_output.operation") == "checkpoint_reconciled" {
				if record.Correlation == nil || record.Correlation.ReceiveOperationID != receiveOperationID {
					t.Fatal("checkpoint reconciliation belongs to a different retained operation")
				}
				checkpointReconciled = true
			}
		case "lane_settlement":
			laneSettlements++
			correlation := record.Correlation
			identity := fmt.Sprintf(
				"%s/%d/%d",
				correlation.ProtocolSessionID,
				*correlation.LaneID,
				*correlation.LaneEpoch,
			)
			if _, duplicate := laneIdentities[identity]; duplicate {
				t.Fatalf("lane settlement %q was emitted more than once", identity)
			}
			laneIdentities[identity] = struct{}{}
		case "lane_adopted":
			laneAdoptions++
		case "receiver_termination":
			if v5TraceStringField(t, record.Payload, "local_stop_reason") == "normal_completion" {
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
	records, _ := readV5UserTrace(t, process.userTracePath, process.userTraceCommand)
	delivered := false
	receiverCompleted := false
	for _, record := range records {
		switch record.Event {
		case "lane_settlement":
			if v5TraceStringField(t, record.Payload, "route") == wantRoute &&
				v5TraceDecimalField(t, record.Payload, "delivered_blocks") > 0 &&
				v5TraceDecimalField(t, record.Payload, "delivered_bytes") > 0 {
				delivered = true
			}
		case "receiver_termination":
			if v5TraceStringField(t, record.Payload, "local_stop_reason") == "normal_completion" {
				receiverCompleted = true
			}
		case "fallback":
			t.Fatalf("transfer using %s emitted an unexpected fallback record", wantRoute)
		case "observer_loss":
			t.Fatalf("transfer using %s lost diagnostic observations: category=%s reason=%s count=%d",
				wantRoute, v5TraceStringField(t, record.Payload, "category"),
				v5TraceStringField(t, record.Payload, "reason"), v5TraceDecimalField(t, record.Payload, "count"))
		}
	}
	if !delivered {
		t.Fatalf("user trace omitted authenticated %s lane delivery", wantRoute)
	}
	if requireReceiverTermination && !receiverCompleted {
		t.Fatal("direct transfer trace omitted normal receiver termination")
	}
}

type v5ProtocolCorrelationKey struct {
	ProtocolSessionID   string
	ProtocolOperationID string
	LaneID              uint32
	LaneEpoch           uint32
}

type v5ProtocolCorrelationEvidence struct {
	engineTaskID string
	runtimeRunID string
	keys         map[v5ProtocolCorrelationKey]struct{}
}

type v5CriticalCorrelationState struct {
	cleanupRegistered bool
	sender            *v5ProtocolCorrelationEvidence
	receivers         map[string]v5ProtocolCorrelationEvidence
}

// Each process seals and validates its own trace when wait completes. Retaining
// only correlation keys by scenario lets the final sender wait prove the join
// without adding a second process scenario or coupling process lifetime code to v4.
var v5CriticalCorrelations = struct {
	sync.Mutex
	states map[*v2Scenario]*v5CriticalCorrelationState
}{states: make(map[*v2Scenario]*v5CriticalCorrelationState)}

func registerCriticalV5ProtocolCorrelation(t *testing.T, process *v2Process, records []v5TraceRecord) {
	t.Helper()
	if process.scenario.operation.Scenario() != v2CriticalRelayTransferScenario {
		return
	}
	evidence := v5ProtocolCorrelationEvidence{
		engineTaskID: assertV5CriticalEngineTask(t, process.userTraceCommand, records),
		runtimeRunID: records[0].RuntimeRunID,
		keys:         make(map[v5ProtocolCorrelationKey]struct{}),
	}
	for _, record := range records {
		correlation := record.Correlation
		if record.Event != "protocol_operation" || correlation == nil ||
			correlation.ProtocolSessionID == "" || correlation.ProtocolOperationID == "" ||
			correlation.LaneID == nil || correlation.LaneEpoch == nil {
			continue
		}
		evidence.keys[v5ProtocolCorrelationKey{
			ProtocolSessionID:   correlation.ProtocolSessionID,
			ProtocolOperationID: correlation.ProtocolOperationID,
			LaneID:              *correlation.LaneID,
			LaneEpoch:           *correlation.LaneEpoch,
		}] = struct{}{}
	}
	if len(evidence.keys) == 0 {
		t.Fatalf("%s trace has no complete protocol session/operation/lane correlation", process.component)
	}

	v5CriticalCorrelations.Lock()
	state := v5CriticalCorrelations.states[process.scenario]
	if state == nil {
		state = &v5CriticalCorrelationState{receivers: make(map[string]v5ProtocolCorrelationEvidence)}
		v5CriticalCorrelations.states[process.scenario] = state
	}
	if !state.cleanupRegistered {
		state.cleanupRegistered = true
		t.Cleanup(func() {
			v5CriticalCorrelations.Lock()
			delete(v5CriticalCorrelations.states, process.scenario)
			v5CriticalCorrelations.Unlock()
		})
	}
	if process.userTraceCommand == "share" {
		state.sender = &evidence
	} else {
		state.receivers[process.userTracePath] = evidence
	}
	sender := state.sender
	receivers := make([]v5ProtocolCorrelationEvidence, 0, len(state.receivers))
	for _, receiver := range state.receivers {
		receivers = append(receivers, receiver)
	}
	v5CriticalCorrelations.Unlock()

	if sender == nil {
		return
	}
	if len(receivers) == 0 {
		t.Fatal("critical sender trace has no receiver trace to correlate")
	}
	for _, receiver := range receivers {
		if sender.engineTaskID == receiver.engineTaskID {
			t.Fatal("sender and receiver unexpectedly share an engine task identity")
		}
		if sender.runtimeRunID == receiver.runtimeRunID {
			t.Fatal("sender and receiver unexpectedly share a local runtime_run_id")
		}
		if !v5TraceCorrelationsIntersect(sender.keys, receiver.keys) {
			t.Fatal("sender and receiver traces have no shared protocol session/operation/lane correlation")
		}
	}
}

func v5TraceCorrelationsIntersect(
	left map[v5ProtocolCorrelationKey]struct{},
	right map[v5ProtocolCorrelationKey]struct{},
) bool {
	for key := range left {
		if _, present := right[key]; present {
			return true
		}
	}
	return false
}

func TestUserTraceAcceptsUnregisteredDiagnosticFields(t *testing.T) {
	record := v5CapacityTraceRecord(1, "get", "future_source_event", v5CapacityBase64ID(1), map[string]any{"new_phase": "active", "nested": map[string]any{"counter": "18446744073709551615", "values": []any{true, "future"}}})
	record["scope"] = map[string]any{"engine_id": "application", "engine_task_id": "application:1", "workflow": "receive"}
	record["source_time"] = "2026-10-25T01:00:00Z"
	record["correlation"] = map[string]any{"future_identity": "source-owned"}
	v5ReadTraceVectors(t, "get", []map[string]any{record})
}
