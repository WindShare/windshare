package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	records, encoded := readV3UserTrace(t, process.userTracePath, process.userTraceCommand)
	for _, forbidden := range process.traceForbidden {
		if v3TraceContainsForbidden(encoded, forbidden) {
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
	incomplete := v3TraceBoolField(t, last.Payload, "incomplete")
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
	assertV3ProducerFactsPrecedeCommandResult(t, process.component, records)
	registerCriticalV3ProtocolCorrelation(t, process, records)
}

func assertV3ProducerFactsPrecedeCommandResult(t *testing.T, component string, records []v3TraceRecord) {
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
		case "relay_lifecycle", "webrtc_lifecycle", "peer_attempt", "receiver_termination", "lane_settlement", "observer_loss":
			t.Fatalf("%s user trace emitted %s after its command result", component, event)
		}
	}
}

func readV3UserTrace(t *testing.T, path, command string) ([]v3TraceRecord, []byte) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read user trace %q: %v", path, err)
	}
	if len(bytes.TrimSpace(encoded)) == 0 {
		t.Fatalf("user trace %q is empty", path)
	}
	lines := bytes.Split(bytes.TrimSpace(encoded), []byte{'\n'})
	records := make([]v3TraceRecord, 0, len(lines))
	sequences := make(map[uint64]struct{}, len(lines))
	runtimeRunID := ""
	for index, line := range lines {
		record := validateV3TraceRecord(t, line, command, sequences)
		if runtimeRunID == "" {
			runtimeRunID = record.RuntimeRunID
		} else if runtimeRunID != record.RuntimeRunID {
			t.Fatalf("user trace line %d changed local runtime_run_id", index+1)
		}
		records = append(records, record)
	}
	return records, encoded
}

func validateV3TraceRecord(
	t *testing.T,
	line []byte,
	command string,
	sequences map[uint64]struct{},
) v3TraceRecord {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var envelope map[string]json.RawMessage
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode user trace record: %v", err)
	}
	if err := ensureV3TraceEOF(decoder); err != nil {
		t.Fatalf("decode user trace record: %v", err)
	}
	allowedEnvelope := map[string]struct{}{
		"schema_version": {}, "sequence": {}, "time": {}, "elapsed_ms": {}, "level": {},
		"event": {}, "command": {}, "runtime_run_id": {}, "correlation": {}, "payload": {},
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
	if version := v3TraceIntegerField(t, envelope, "schema_version"); version != v3TraceSchemaVersion {
		t.Fatalf("user trace schema_version=%d want=%d", version, v3TraceSchemaVersion)
	}
	sequence := v3TraceDecimalField(t, envelope, "sequence")
	if sequence == 0 {
		t.Fatal("user trace sequence is zero")
	}
	if _, duplicate := sequences[sequence]; duplicate {
		t.Fatalf("user trace sequence %d is duplicated", sequence)
	}
	sequences[sequence] = struct{}{}
	_ = v3TraceDecimalField(t, envelope, "elapsed_ms")
	if _, err := time.Parse(time.RFC3339Nano, v3TraceStringField(t, envelope, "time")); err != nil {
		t.Fatalf("user trace time is invalid: %v", err)
	}
	if got := v3TraceStringField(t, envelope, "command"); got != command {
		t.Fatalf("user trace command=%q want=%q", got, command)
	}
	runtimeRunID := v3TraceStringField(t, envelope, "runtime_run_id")
	validateV3TraceIdentity(t, runtimeRunID, "runtime_run_id", v3IdentityBytes)
	event := v3TraceStringField(t, envelope, "event")
	schema, known := v3TracePayloadSchemas[event]
	if !known {
		t.Fatalf("user trace event %q is outside the closed vocabulary", event)
	}
	switch level := v3TraceStringField(t, envelope, "level"); level {
	case "debug", "info", "warn", "error":
	default:
		t.Fatalf("user trace level %q is outside the closed vocabulary", level)
	}
	payload := v3TraceObjectMap(t, envelope["payload"], event+" payload")
	validateV3TraceObject(t, payload, schema, event+" payload")
	var correlation *v3TraceCorrelation
	if raw, present := envelope["correlation"]; present {
		projected := validateV3TraceCorrelation(t, raw, event+" correlation")
		correlation = &projected
	}
	record := v3TraceRecord{
		Event: event, RuntimeRunID: runtimeRunID, Correlation: correlation, Payload: payload,
	}
	validateV3DiagnosticRecord(t, record)
	return record
}

func ensureV3TraceEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateV3TraceObject(
	t *testing.T,
	object map[string]json.RawMessage,
	schema *v3TraceObjectSchema,
	context string,
) {
	t.Helper()
	for name, raw := range object {
		field, known := schema.fields[name]
		if !known {
			t.Fatalf("%s contains unknown field %q", context, name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			t.Fatalf("%s field %q is null", context, name)
		}
		validateV3TraceValue(t, raw, field, context)
	}
	for name, field := range schema.fields {
		if _, present := object[name]; !present && !field.optional {
			t.Fatalf("%s is missing field %q", context, name)
		}
	}
}

func validateV3TraceValue(t *testing.T, raw json.RawMessage, field v3TraceFieldSchema, context string) {
	t.Helper()
	fieldContext := context + "." + field.name
	switch field.kind {
	case v3TraceString:
		if value := v3TraceStringRaw(t, raw, fieldContext); value == "" {
			t.Fatalf("%s is empty", fieldContext)
		}
	case v3TraceIdentity:
		validateV3TraceIdentity(t, v3TraceStringRaw(t, raw, fieldContext), fieldContext, v3IdentityBytes)
	case v3TraceRelaySessionIdentity:
		validateV3TraceIdentity(t, v3TraceStringRaw(t, raw, fieldContext), fieldContext, 8)
	case v3TraceDecimal:
		_ = v3TraceDecimalRaw(t, raw, fieldContext)
	case v3TraceInteger:
		_ = v3TraceIntegerRaw(t, raw, fieldContext)
	case v3TraceBool:
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s is not a bool: %v", fieldContext, err)
		}
	case v3TraceStringSlice:
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			t.Fatalf("%s is not a string array: %v", fieldContext, err)
		}
		for _, value := range values {
			if value == "" {
				t.Fatalf("%s contains an empty value", fieldContext)
			}
		}
	case v3TraceObject:
		object := v3TraceObjectMap(t, raw, fieldContext)
		validateV3TraceObject(t, object, field.object, fieldContext)
	case v3TraceHexIdentity:
		validateV3TraceHex(t, v3TraceStringRaw(t, raw, fieldContext), fieldContext, v3IdentityBytes)
	case v3TraceHexDigest:
		validateV3TraceHex(t, v3TraceStringRaw(t, raw, fieldContext), fieldContext, v3DigestBytes)
	case v3TraceCorrelationValue:
		_ = validateV3TraceCorrelation(t, raw, fieldContext)
	case v3TraceProtocolSettlement:
		validateV3ProtocolSettlement(t, raw, fieldContext)
	default:
		t.Fatalf("%s has unsupported schema kind %d", fieldContext, field.kind)
	}
}

func validateV3ProtocolSettlement(t *testing.T, raw json.RawMessage, context string) {
	t.Helper()
	settlement := v3TraceObjectMap(t, raw, context)
	kind := v3TraceStringField(t, settlement, "kind")
	var schema *v3TraceObjectSchema
	switch kind {
	case "received_authenticated":
		schema = v3TraceSchema(v3TraceFields(v3TraceString, "kind"))
	case "response_send":
		schema = v3TraceSchema(
			v3TraceFields(v3TraceString, "kind", "outcome"),
			v3TraceFields(v3TraceBool, "admitted", "settled"),
		)
	default:
		t.Fatalf("%s has unknown kind %q", context, kind)
	}
	validateV3TraceObject(t, settlement, schema, context)
}

func validateV3TraceCorrelation(t *testing.T, raw json.RawMessage, context string) v3TraceCorrelation {
	t.Helper()
	object := v3TraceObjectMap(t, raw, context)
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
	correlation := v3TraceCorrelation{}
	for field, target := range map[string]*string{
		"protocol_session_id":   &correlation.ProtocolSessionID,
		"protocol_operation_id": &correlation.ProtocolOperationID,
		"peer_path_id":          &correlation.PeerPathID,
		"peer_attempt_id":       &correlation.PeerAttemptID,
	} {
		if value, present := object[field]; present {
			*target = v3TraceStringRaw(t, value, context+"."+field)
			validateV3TraceIdentity(t, *target, context+"."+field, v3IdentityBytes)
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
		laneID := v3TraceIntegerRaw(t, laneIDRaw, context+".lane_id")
		laneEpoch := v3TraceIntegerRaw(t, laneEpochRaw, context+".lane_epoch")
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

func validateV3DiagnosticRecord(t *testing.T, record v3TraceRecord) {
	t.Helper()
	switch record.Event {
	case "lane_adopted", "lane_settlement":
		if record.Correlation == nil || record.Correlation.ProtocolSessionID == "" ||
			record.Correlation.LaneID == nil || record.Correlation.LaneEpoch == nil {
			t.Fatalf("%s lacks protocol session and lane correlation", record.Event)
		}
	case "protocol_operation":
		if record.Correlation == nil || record.Correlation.ProtocolSessionID == "" ||
			record.Correlation.ProtocolOperationID == "" {
			t.Fatal("protocol operation lacks shared session/operation correlation")
		}
	case "filesystem_output":
		if raw, present := record.Payload["checkpoint_decision"]; present {
			decision := v3TraceStringRaw(t, raw, "filesystem_output.checkpoint_decision")
			if !v3ValidFilesystemCheckpointDecision(record.Event, decision) {
				t.Fatalf(
					"filesystem checkpoint decision %q is outside the closed vocabulary",
					decision,
				)
			}
		}
	case "observer_loss":
		if !v3KnownObserverLossCategory(v3TraceStringField(t, record.Payload, "category")) {
			t.Fatal("observer loss category is outside the closed vocabulary")
		}
		if !v3KnownObserverLossReason(v3TraceStringField(t, record.Payload, "reason")) {
			t.Fatal("observer loss reason is outside the closed vocabulary")
		}
		if v3TraceDecimalField(t, record.Payload, "count") == 0 {
			t.Fatal("observer loss count is zero")
		}
	case "receiver_termination":
		switch reason := v3TraceStringField(t, record.Payload, "local_stop_reason"); reason {
		case "none", "caller_stop", "output_admission_stop", "runtime_session_failure", "normal_completion":
		default:
			t.Fatalf("receiver termination has unknown local stop reason %q", reason)
		}
	case "relay_lifecycle":
		if v3TraceStringField(t, record.Payload, "stage") == "send_admitted" {
			t.Fatal("ordinary successful relay sends must not enter the user trace")
		}
	}
}

func v3ValidFilesystemCheckpointDecision(event, decision string) bool {
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

func v3KnownObserverLossCategory(value string) bool {
	switch value {
	case "relay_lifecycle", "webrtc_lifecycle", "sender_attempt", "receiver_termination", "lane_settlement",
		"protocol_operation", "transfer_lifecycle", "filesystem_output", "catalog_storage", "root_prefetch",
		"sender_capacity", "sender_revision", "command_adapter":
		return true
	default:
		return false
	}
}

func v3KnownObserverLossReason(value string) bool {
	switch value {
	case "unknown_enum", "invalid_identity", "invalid_stage_field_combination", "event_contract_rejection",
		"adapter_capacity_timeout", "trace_queue", "recorder_closed":
		return true
	default:
		return false
	}
}

func v3TraceObjectMap(t *testing.T, raw json.RawMessage, context string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		t.Fatalf("%s is not an object: %v", context, err)
	}
	return object
}

func v3TraceStringField(t *testing.T, object map[string]json.RawMessage, field string) string {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v3TraceStringRaw(t, raw, field)
}

func v3TraceStringRaw(t *testing.T, raw json.RawMessage, context string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("user trace field %s is not a string: %v", context, err)
	}
	return value
}

func v3TraceBoolField(t *testing.T, object map[string]json.RawMessage, field string) bool {
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

func v3TraceIntegerField(t *testing.T, object map[string]json.RawMessage, field string) uint64 {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v3TraceIntegerRaw(t, raw, field)
}

func v3TraceIntegerRaw(t *testing.T, raw json.RawMessage, context string) uint64 {
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

func v3TraceDecimalField(t *testing.T, object map[string]json.RawMessage, field string) uint64 {
	t.Helper()
	raw, present := object[field]
	if !present {
		t.Fatalf("user trace is missing %q", field)
	}
	return v3TraceDecimalRaw(t, raw, field)
}

func v3TraceDecimalRaw(t *testing.T, raw json.RawMessage, context string) uint64 {
	t.Helper()
	value := v3TraceStringRaw(t, raw, context)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		t.Fatalf("user trace field %s is not a canonical unsigned decimal string: %q", context, value)
	}
	return parsed
}

func validateV3TraceIdentity(t *testing.T, value, context string, wantBytes int) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != wantBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		t.Fatalf("user trace field %s is not a canonical unpadded base64url identity: %q", context, value)
	}
}

func v3TraceContainsForbidden(encoded []byte, forbidden string) bool {
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
	records, _ := readV3UserTrace(t, process.userTracePath, process.userTraceCommand)
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
	receiveOperationID = v3TraceIdentityFromHex(t, receiveOperationID)
	records, _ := readV3UserTrace(t, process.userTracePath, process.userTraceCommand)
	checkpointReconciled := false
	receiverCompleted := false
	laneSettlements := 0
	laneAdoptions := 0
	laneIdentities := make(map[string]struct{})
	for _, record := range records {
		switch record.Event {
		case "filesystem_output":
			if operation, ok := record.Payload["operation"]; ok &&
				v3TraceStringRaw(t, operation, "filesystem_output.operation") == "checkpoint_reconciled" {
				if v3TraceStringField(t, record.Payload, "receive_operation_id") != receiveOperationID {
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
			if v3TraceStringField(t, record.Payload, "local_stop_reason") == "normal_completion" {
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
	records, _ := readV3UserTrace(t, process.userTracePath, process.userTraceCommand)
	delivered := false
	receiverCompleted := false
	for _, record := range records {
		switch record.Event {
		case "lane_settlement":
			if v3TraceStringField(t, record.Payload, "route") == wantRoute &&
				v3TraceDecimalField(t, record.Payload, "delivered_blocks") > 0 &&
				v3TraceDecimalField(t, record.Payload, "delivered_bytes") > 0 {
				delivered = true
			}
		case "receiver_termination":
			if v3TraceStringField(t, record.Payload, "local_stop_reason") == "normal_completion" {
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

type v3ProtocolCorrelationKey struct {
	ProtocolSessionID   string
	ProtocolOperationID string
	LaneID              uint32
	LaneEpoch           uint32
}

type v3ProtocolCorrelationEvidence struct {
	runtimeRunID string
	keys         map[v3ProtocolCorrelationKey]struct{}
}

type v3CriticalCorrelationState struct {
	cleanupRegistered bool
	sender            *v3ProtocolCorrelationEvidence
	receivers         map[string]v3ProtocolCorrelationEvidence
}

// Each process seals and validates its own trace when wait completes. Retaining
// only correlation keys by scenario lets the final sender wait prove the join
// without adding a second process scenario or coupling process lifetime code to v3.
var v3CriticalCorrelations = struct {
	sync.Mutex
	states map[*v2Scenario]*v3CriticalCorrelationState
}{states: make(map[*v2Scenario]*v3CriticalCorrelationState)}

func registerCriticalV3ProtocolCorrelation(t *testing.T, process *v2Process, records []v3TraceRecord) {
	t.Helper()
	if process.scenario.operation.Scenario() != v2CriticalRelayTransferScenario {
		return
	}
	evidence := v3ProtocolCorrelationEvidence{
		runtimeRunID: records[0].RuntimeRunID,
		keys:         make(map[v3ProtocolCorrelationKey]struct{}),
	}
	for _, record := range records {
		correlation := record.Correlation
		if record.Event != "protocol_operation" || correlation == nil ||
			correlation.ProtocolSessionID == "" || correlation.ProtocolOperationID == "" ||
			correlation.LaneID == nil || correlation.LaneEpoch == nil {
			continue
		}
		evidence.keys[v3ProtocolCorrelationKey{
			ProtocolSessionID:   correlation.ProtocolSessionID,
			ProtocolOperationID: correlation.ProtocolOperationID,
			LaneID:              *correlation.LaneID,
			LaneEpoch:           *correlation.LaneEpoch,
		}] = struct{}{}
	}
	if len(evidence.keys) == 0 {
		t.Fatalf("%s trace has no complete protocol session/operation/lane correlation", process.component)
	}

	v3CriticalCorrelations.Lock()
	state := v3CriticalCorrelations.states[process.scenario]
	if state == nil {
		state = &v3CriticalCorrelationState{receivers: make(map[string]v3ProtocolCorrelationEvidence)}
		v3CriticalCorrelations.states[process.scenario] = state
	}
	if !state.cleanupRegistered {
		state.cleanupRegistered = true
		t.Cleanup(func() {
			v3CriticalCorrelations.Lock()
			delete(v3CriticalCorrelations.states, process.scenario)
			v3CriticalCorrelations.Unlock()
		})
	}
	if process.userTraceCommand == "share" {
		state.sender = &evidence
	} else {
		state.receivers[process.userTracePath] = evidence
	}
	sender := state.sender
	receivers := make([]v3ProtocolCorrelationEvidence, 0, len(state.receivers))
	for _, receiver := range state.receivers {
		receivers = append(receivers, receiver)
	}
	v3CriticalCorrelations.Unlock()

	if sender == nil {
		return
	}
	if len(receivers) == 0 {
		t.Fatal("critical sender trace has no receiver trace to correlate")
	}
	for _, receiver := range receivers {
		if sender.runtimeRunID == receiver.runtimeRunID {
			t.Fatal("sender and receiver unexpectedly share a local runtime_run_id")
		}
		if !v3TraceCorrelationsIntersect(sender.keys, receiver.keys) {
			t.Fatal("sender and receiver traces have no shared protocol session/operation/lane correlation")
		}
	}
}

func v3TraceCorrelationsIntersect(
	left map[v3ProtocolCorrelationKey]struct{},
	right map[v3ProtocolCorrelationKey]struct{},
) bool {
	for key := range left {
		if _, present := right[key]; present {
			return true
		}
	}
	return false
}

func TestUserTraceV3DiagnosticContract(t *testing.T) {
	runtimeRunID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, v3IdentityBytes))
	protocolSessionID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, v3IdentityBytes))
	protocolOperationID := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x33}, v3IdentityBytes))
	base := func(sequence int, event string, payload map[string]any) map[string]any {
		return map[string]any{
			"schema_version": v3TraceSchemaVersion,
			"sequence":       strconv.Itoa(sequence),
			"time":           "2026-08-17T00:00:00Z",
			"elapsed_ms":     strconv.Itoa(sequence),
			"level":          "debug",
			"event":          event,
			"command":        "get",
			"runtime_run_id": runtimeRunID,
			"payload":        payload,
		}
	}
	lane := base(1, "lane_settlement", map[string]any{
		"route": "relay", "delivered_blocks": "2", "delivered_bytes": "8192",
		"failed_block_attempts": "0", "reassigned_blocks": "0", "incomplete": false,
	})
	lane["correlation"] = map[string]any{
		"protocol_session_id": protocolSessionID, "lane_id": 1, "lane_epoch": 0,
	}
	protocol := base(2, "protocol_operation", map[string]any{
		"role": "receiver", "stage": "receiver_settled", "request_kind": "request_file",
		"response_count": "1", "operation_elapsed_ms": "2", "usable_lanes_at_selection": 1,
		"usable_lanes_at_settlement": 1, "cause": "completed",
	})
	protocol["correlation"] = map[string]any{
		"protocol_session_id": protocolSessionID, "protocol_operation_id": protocolOperationID,
		"lane_id": 1, "lane_epoch": 0,
	}
	loss := base(3, "observer_loss", map[string]any{
		"category": "filesystem_output", "reason": "trace_queue", "count": "1",
	})
	termination := base(4, "receiver_termination", map[string]any{
		"local_generation": "1", "transition_authority": "local", "disposition": "session_unavailable",
		"transition_provenance": "local_explicit_stop", "consequence_provenance": "local_explicit_stop",
		"local_stop_reason": "output_admission_stop", "diagnostics_truncated": false,
		"benign_components": []string{}, "retained_cause_classes": []string{}, "teardown_transitions": []string{},
		"peer_shutdown_failed": false, "channel_drain_failed": false,
	})
	adaptive := base(5, "content_path_selected", map[string]any{"content_path": "direct_and_relay"})
	recordVectors := []map[string]any{lane, protocol, loss, termination, adaptive}
	for index, decision := range []string{
		"absent", "exact", "revision_conflict", "ownership_conflict", "invalid",
	} {
		filesystem := base(index+6, "filesystem_output", map[string]any{
			"operation":           "runtime_decision",
			"checkpoint_decision": decision,
			"counters": map[string]any{
				"node_claims": "0", "directory_claims": "0", "file_claims": "0",
				"active_file_claims": "0", "reserved_file_slots": "0",
				"directory_metadata_bytes": "0", "checkpoint_records": "1",
			},
		})
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
	records, _ := readV3UserTrace(t, path, "get")
	if len(records) != len(recordVectors) {
		t.Fatalf("diagnostic records=%d want=%d", len(records), len(recordVectors))
	}
	for _, record := range records {
		if record.Event == "fallback" {
			t.Fatal("adaptive relay admission or output-admission shutdown produced fallback evidence")
		}
	}
}
