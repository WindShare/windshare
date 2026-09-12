package e2e

import "testing"

func TestUserTraceV4SenderResponseFacts(t *testing.T) {
	runID := v4CapacityBase64ID(1)
	session := v4CapacityBase64ID(2)
	operation := v4CapacityBase64ID(3)
	correlation := map[string]any{"protocol_session_id": session, "protocol_operation_id": operation}
	attempt := map[string]any{"attempt_sequence": "1", "lane_id": 7, "lane_epoch": 2, "policy_admitted": true, "settled": false, "outcome": "unknown", "end": "waiting_ended", "cause": map[string]any{"kind": "deadline", "detail": "context deadline exceeded", "truncated": false}}
	content := map[string]any{"scope": "revision", "code": 12296, "retryable": true, "retry_after_ms": 125}
	returned := v4CapacityTraceRecord(2, "share", "protocol_response_send_returned", runID, map[string]any{"observed_at": "2026-08-23T00:00:00.123Z", "role": "sender", "request_kind": "open_revisions", "response_kind": "operation_error", "response_sequence": "41", "protocol_error": content, "response_result": map[string]any{"started": true, "evidence": "uncertain", "end": "deadline_exceeded", "cleanup": "route_released", "attempts": []any{attempt}, "pending_attempt_sequence": "1"}})
	returned["correlation"] = correlation
	settledAttempt := map[string]any{"attempt_sequence": "1", "lane_id": 7, "lane_epoch": 2, "policy_admitted": true, "settled": true, "outcome": "transport_confirmed", "transport_disposition": "accepted", "end": "settled", "cause": map[string]any{"kind": "none", "truncated": false}}
	settled := v4CapacityTraceRecord(1, "share", "protocol_send_attempt_settled", runID, map[string]any{"observed_at": "2026-08-23T00:00:01Z", "role": "sender", "request_kind": "open_revisions", "response_kind": "operation_error", "response_sequence": "41", "attempt": settledAttempt})
	settled["correlation"] = map[string]any{"protocol_session_id": session, "protocol_operation_id": operation, "lane_id": 7, "lane_epoch": 2}
	notStarted := v4CapacityTraceRecord(3, "share", "protocol_response_send_not_started", runID, map[string]any{"observed_at": "2026-08-23T00:00:02Z", "role": "sender", "response_kind": "operation_error", "response_sequence": "42", "protocol_error": content, "response_result": map[string]any{"started": false, "evidence": "definitely_not_sent", "end": "route_unavailable", "cleanup": "none", "attempts": []any{}}})
	notStarted["correlation"] = correlation
	rejection := v4CapacityTraceRecord(4, "share", "observer_loss", runID, map[string]any{"category": "protocol_operation", "reason": "unknown_enum", "count": "9", "omitted_samples": "0", "rejection": map[string]any{"source_event": "protocol_response_send_returned", "source_location": "commandprojection.ProjectProtocolObservation", "source_stage": "response_send_returned", "field": "role", "rule": "known_enum", "sample_protocol_session_id": "00000000000000000000000000000000", "sample_response_sequence": "41", "evidence": []any{map[string]any{"field": "role", "representation": "enum_number", "value": "255"}}, "truncated": true, "omitted_fields": "3", "omitted_bytes": "31"}})
	v4ReadTraceVectors(t, "share", []map[string]any{settled, returned, notStarted, rejection})
}
