package e2e

import (
	"crypto/sha256"
	"encoding/json"
)

const (
	v4TraceSchemaVersion = 4
	v4IdentityBytes      = 16
	v4DigestBytes        = sha256.Size
)

type v4TraceValueKind uint8

const (
	v4TraceString v4TraceValueKind = iota + 1
	v4TraceIdentity
	v4TraceRelaySessionIdentity
	v4TraceDecimal
	v4TraceInteger
	v4TraceBool
	v4TraceStringSlice
	v4TraceObject
	v4TraceHexIdentity
	v4TraceHexDigest
	v4TraceCorrelationValue
	v4TraceObjectSlice
	v4TraceTimestamp
	v4TraceRawString
	v4TraceFraction
)

type v4TraceFieldSchema struct {
	name     string
	kind     v4TraceValueKind
	object   *v4TraceObjectSchema
	optional bool
	nullable bool
}

type v4TraceObjectSchema struct {
	fields map[string]v4TraceFieldSchema
}

type v4TraceCorrelation struct {
	ProtocolSessionID   string
	ProtocolOperationID string
	PeerPathID          string
	PeerAttemptID       string
	LaneID              *uint32
	LaneEpoch           *uint32
}

type v4TraceRecord struct {
	Event        string
	RuntimeRunID string
	Correlation  *v4TraceCorrelation
	Payload      map[string]json.RawMessage
}

var v4TracePayloadSchemas = buildV4TracePayloadSchemas()

func v4TraceSchema(groups ...[]v4TraceFieldSchema) *v4TraceObjectSchema {
	fields := make(map[string]v4TraceFieldSchema)
	for _, group := range groups {
		for _, field := range group {
			if _, duplicate := fields[field.name]; duplicate {
				panic("duplicate v4 trace schema field: " + field.name)
			}
			fields[field.name] = field
		}
	}
	return &v4TraceObjectSchema{fields: fields}
}

func v4TraceFields(kind v4TraceValueKind, names ...string) []v4TraceFieldSchema {
	fields := make([]v4TraceFieldSchema, 0, len(names))
	for _, name := range names {
		fields = append(fields, v4TraceFieldSchema{name: name, kind: kind})
	}
	return fields
}

func v4TraceOptionalFields(kind v4TraceValueKind, names ...string) []v4TraceFieldSchema {
	fields := v4TraceFields(kind, names...)
	for index := range fields {
		fields[index].optional = true
	}
	return fields
}

func v4TraceObjectField(name string, object *v4TraceObjectSchema, optional bool) []v4TraceFieldSchema {
	return []v4TraceFieldSchema{{name: name, kind: v4TraceObject, object: object, optional: optional}}
}

func buildV4TracePayloadSchemas() map[string]*v4TraceObjectSchema {
	relayAuthority := v4TraceSchema(
		v4TraceFields(v4TraceString, "scheme", "host"),
		v4TraceFields(v4TraceInteger, "port"),
	)
	fault := v4TraceSchema(
		v4TraceFields(v4TraceString, "domain", "scope"),
		v4TraceFields(v4TraceInteger, "code"),
	)
	failure := v4TraceSchema(
		v4TraceFields(v4TraceString, "code", "message_key"),
		v4TraceObjectField("fault", fault, true),
		v4TraceOptionalFields(v4TraceDecimal, "retry_after_ms"),
	)
	fileOutcomes := v4TraceSchema(v4TraceFields(
		v4TraceDecimal,
		"downloaded_files", "resumed_files", "previously_published_files", "paused_files", "collision_files",
		"item_blocked_files", "failed_files", "modified_time_warnings",
	))
	capacityWait := v4TraceSchema(v4TraceFields(
		v4TraceDecimal, "active_waiters", "accumulated_wait_ms", "attempts",
	))
	progress := v4TraceSchema(
		v4TraceFields(v4TraceString, "discovery"),
		v4TraceFields(v4TraceBool, "counters_exact"),
		v4TraceFields(
			v4TraceDecimal,
			"discovered_files", "discovered_bytes", "published_files", "published_bytes",
			"verified_bytes", "newly_verified_bytes", "previously_published_bytes",
		),
		v4TraceObjectField("file_outcomes", fileOutcomes, false),
		v4TraceObjectField("capacity_wait", capacityWait, false),
	)
	capacityScope := v4TraceSchema(v4TraceFields(
		v4TraceDecimal,
		"stable_handles", "active_leases", "stable_handle_limit", "active_lease_limit",
		"reclaimable_stable_handles", "quarantined_stable_handles", "pending_admissions", "active_reclaims",
	))
	transferCapacity := v4TraceSchema(
		v4TraceFields(v4TraceIdentity, "wait_id", "generation_id", "protocol_operation_id"),
		v4TraceFields(v4TraceDecimal, "attempt", "hint_ms", "jitter_ms", "delay_ms", "accumulated_wait_ms"),
		v4TraceFields(v4TraceInteger, "active_waiters"),
	)
	senderContentDecision := v4TraceSchema(
		v4TraceFields(v4TraceString, "kind"),
		v4TraceOptionalFields(v4TraceHexDigest, "capacity_decision_id"),
		v4TraceOptionalFields(v4TraceHexIdentity, "lease_id"),
	)
	peerPhaseDeadline := v4TraceSchema(
		v4TraceFields(v4TraceString, "phase"),
		v4TraceFields(v4TraceDecimal, "deadline_ms"),
	)
	peerCandidates := v4TraceSchema(v4TraceFields(v4TraceInteger, "local_emitted", "remote_accepted"))
	peerAdmission := v4TraceSchema(v4TraceFields(v4TraceString, "disposition", "response_delivery"))
	peerRejection := v4TraceSchema(
		v4TraceFields(v4TraceString, "code"),
		v4TraceOptionalFields(v4TraceDecimal, "retry_after_ms"),
	)
	peerFailure := v4TraceSchema(
		v4TraceFields(v4TraceString, "failed_at_stage", "scope"),
		v4TraceObjectField("failure", failure, false),
	)
	filesystemNativeLock := v4TraceSchema(v4TraceFields(v4TraceString, "scope", "milestone"))
	filesystemRuntimeDecision := v4TraceSchema(v4TraceFields(v4TraceString, "component", "operation", "decision"))
	filesystemCorrelation := v4TraceSchema(v4TraceOptionalFields(v4TraceDecimal, "operation_id", "claim_id"))
	filesystemCapability := v4TraceSchema(
		v4TraceFields(v4TraceBool, "supported"),
		v4TraceFields(v4TraceString, "reason"),
	)
	filesystemCapabilities := v4TraceSchema(
		v4TraceFields(v4TraceString, "mode"),
		v4TraceObjectField("safe_publish", filesystemCapability, false),
		v4TraceObjectField("operation_recovery", filesystemCapability, false),
		v4TraceObjectField("range_recovery", filesystemCapability, false),
		v4TraceObjectField("crash_cleanup", filesystemCapability, false),
	)
	filesystemCounters := v4TraceSchema(v4TraceFields(
		v4TraceDecimal,
		"node_claims", "directory_claims", "file_claims", "active_file_claims",
		"reserved_file_slots", "directory_metadata_bytes", "checkpoint_records",
	))
	filesystemFailure := v4TraceSchema(
		v4TraceFields(v4TraceString, "stage"),
		v4TraceOptionalFields(v4TraceString, "reconciliation_step", "native_error_class"),
		v4TraceObjectField("failure", failure, false),
	)
	catalogUsage := v4TraceSchema(v4TraceFields(
		v4TraceDecimal,
		"active_scans", "scan_work", "entries", "memory_bytes", "spill_bytes",
	))
	protocolSend := v4TraceSchema(
		v4TraceFields(v4TraceBool, "settled", "admitted"),
		v4TraceFields(v4TraceString, "outcome"),
	)
	protocolError := v4TraceSchema(v4TraceFields(v4TraceString, "scope"), v4TraceFields(v4TraceInteger, "code"), v4TraceFields(v4TraceBool, "retryable"), v4TraceOptionalFields(v4TraceInteger, "retry_after_ms"))
	attemptCause := v4TraceSchema(v4TraceFields(v4TraceString, "kind"), v4TraceOptionalFields(v4TraceRawString, "detail"), v4TraceFields(v4TraceBool, "truncated"))
	attempt := v4TraceSchema(v4TraceObjectField("cause", attemptCause, false), v4TraceFields(v4TraceDecimal, "attempt_sequence"), v4TraceFields(v4TraceInteger, "lane_id", "lane_epoch"), v4TraceFields(v4TraceBool, "policy_admitted", "settled"), v4TraceFields(v4TraceString, "outcome", "end"), v4TraceOptionalFields(v4TraceString, "transport_disposition"))
	responseResult := v4TraceSchema(v4TraceFields(v4TraceBool, "started"), v4TraceFields(v4TraceString, "evidence", "end", "cleanup"), v4TraceOptionalFields(v4TraceDecimal, "pending_attempt_sequence"), v4TraceObjectSliceField("attempts", attempt))
	response := v4TraceSchema(v4TraceFields(v4TraceTimestamp, "observed_at"), v4TraceFields(v4TraceString, "role", "request_kind", "response_kind"), v4TraceFields(v4TraceDecimal, "response_sequence"), v4TraceObjectField("protocol_error", protocolError, true), v4TraceObjectField("response_result", responseResult, false))
	notStartedResponse := v4TraceSchema(v4TraceFields(v4TraceTimestamp, "observed_at"), v4TraceFields(v4TraceString, "role", "response_kind"), v4TraceOptionalFields(v4TraceString, "request_kind"), v4TraceFields(v4TraceDecimal, "response_sequence"), v4TraceObjectField("protocol_error", protocolError, true), v4TraceObjectField("response_result", responseResult, false))
	rejectionField := v4TraceSchema(v4TraceFields(v4TraceString, "field", "representation"), v4TraceFields(v4TraceRawString, "value"))
	rejection := v4TraceSchema(v4TraceFields(v4TraceString, "source_event", "source_location", "source_stage", "field", "rule"), v4TraceOptionalFields(v4TraceRawString, "sample_protocol_session_id", "sample_protocol_operation_id", "sample_revision_id", "sample_response_sequence", "sample_attempt_sequence"), v4TraceObjectSliceField("evidence", rejectionField), v4TraceFields(v4TraceBool, "truncated"), v4TraceFields(v4TraceDecimal, "omitted_fields", "omitted_bytes"))

	return map[string]*v4TraceObjectSchema{
		"ready":               v4TraceSchema(),
		"platform_setup":      v4TraceSchema(v4TraceFields(v4TraceString, "state", "reason")),
		"native_connectivity": v4NativeConnectivitySchema(),
		"sharing_subject_selected": v4TraceSchema(
			v4TraceFields(v4TraceString, "subject_kind"),
			v4TraceFields(v4TraceDecimal, "selected_items"),
			v4TraceOptionalFields(v4TraceDecimal, "file_bytes"),
		),
		"relay_connected": v4TraceSchema(v4TraceObjectField("relay_authority", relayAuthority, false)),
		"relay_recovering": v4TraceSchema(
			v4TraceObjectField("relay_authority", relayAuthority, false),
			v4TraceFields(v4TraceInteger, "attempt"),
			v4TraceFields(v4TraceString, "state"),
			v4TraceObjectField("failure", failure, true),
		),
		"content_path_selected": v4TraceSchema(v4TraceFields(v4TraceString, "content_path")),
		"fallback": v4TraceSchema(
			v4TraceFields(v4TraceString, "from_transport", "to_transport"),
			v4TraceObjectField("failure", failure, false),
		),
		"transfer_progress": v4TraceSchema(
			v4TraceFields(v4TraceIdentity, "receive_operation_id", "transfer_job_id"),
			v4TraceObjectField("progress", progress, false),
		),
		"warning": v4TraceSchema(v4TraceObjectField("failure", failure, false)),
		"command_failed": v4TraceSchema(
			v4TraceFields(v4TraceInteger, "exit_code"),
			v4TraceObjectField("failure", failure, false),
		),
		"transfer_settled": v4TraceSchema(
			v4TraceObjectField("download_connectivity", v4DownloadConnectivitySchema(), true),
			v4TraceFields(v4TraceString, "result_status", "drift"),
			v4TraceFields(v4TraceInteger, "exit_code"),
			v4TraceFields(v4TraceDecimal, "result_elapsed_ms", "directory_failures", "omitted_diagnostics", "published_bytes"),
			v4TraceFields(v4TraceBool, "destination_adjusted", "counters_exact"),
			v4TraceObjectField("file_outcomes", fileOutcomes, false),
			v4TraceObjectField("failure", failure, true),
		),
		"sharing_stopped": v4TraceSchema(
			v4TraceFields(v4TraceInteger, "exit_code"),
			v4TraceFields(v4TraceDecimal, "result_elapsed_ms"),
			v4TraceFields(v4TraceBool, "stopped_cleanly"),
			v4TraceObjectField("failure", failure, true),
		),
		"trace_incomplete": v4TraceSchema(
			v4TraceFields(v4TraceString, "cause"),
			v4TraceFields(v4TraceDecimal, "lifecycle_dropped", "progress_dropped"),
		),
		"lane_adopted": v4TraceSchema(v4TraceFields(v4TraceString, "transport")),
		"relay_lifecycle": v4TraceSchema(
			v4TraceFields(v4TraceDecimal, "link_id"),
			v4TraceOptionalFields(v4TraceRelaySessionIdentity, "relay_session_id"),
			v4TraceOptionalFields(v4TraceDecimal, "send_operation_id", "dropped"),
			v4TraceFields(v4TraceString, "stage", "retirement_source", "cause", "drain_cause"),
			v4TraceOptionalFields(v4TraceString, "disposition"),
			v4TraceFields(v4TraceBool, "terminal"),
		),
		"webrtc_lifecycle": v4TraceSchema(
			v4TraceFields(v4TraceDecimal, "channel_id"),
			v4TraceOptionalFields(v4TraceDecimal, "send_operation_id", "dropped"),
			v4TraceFields(v4TraceString, "operation", "transition", "state", "terminal_state", "cause"),
			v4TraceOptionalFields(v4TraceString, "disposition"),
		),
		"peer_attempt": v4TraceSchema(
			v4TraceFields(v4TraceDecimal, "attempt_sequence", "attempt_elapsed_ms"),
			v4TraceFields(v4TraceString, "stage"),
			v4TraceOptionalFields(v4TraceIdentity, "offer_operation_id", "grant_operation_id"),
			v4TraceObjectField("phase_deadline", peerPhaseDeadline, true),
			v4TraceObjectField("candidates", peerCandidates, true),
			v4TraceObjectField("admission", peerAdmission, true),
			v4TraceObjectField("rejection", peerRejection, true),
			v4TraceObjectField("failure", peerFailure, true),
		),
		"transfer_lifecycle": v4TraceSchema(
			v4TraceFields(v4TraceIdentity, "receive_operation_id", "transfer_job_id"),
			v4TraceFields(v4TraceString, "stage", "file_selection", "file_settlement", "tree_settlement"),
			v4TraceOptionalFields(v4TraceString, "item_block_reason"),
			v4TraceObjectField("progress", progress, false),
			v4TraceObjectField("capacity", transferCapacity, true),
			v4TraceObjectField("failure", failure, true),
		),
		"sender_capacity": v4TraceSchema(
			v4TraceFields(v4TraceString, "stage"),
			v4TraceOptionalFields(v4TraceHexDigest, "decision_id", "revision_id"),
			v4TraceObjectField("process", capacityScope, false),
			v4TraceObjectField("share", capacityScope, true),
			v4TraceObjectField("session", capacityScope, true),
		),
		"sender_revision": v4TraceSchema(
			v4TraceFields(v4TraceString, "stage", "cause"),
			v4TraceFields(v4TraceHexDigest, "revision_id"),
			v4TraceOptionalFields(v4TraceHexIdentity, "lease_id"),
		),
		"filesystem_output": v4TraceSchema(
			v4TraceFields(v4TraceString, "operation"),
			v4TraceOptionalFields(v4TraceIdentity, "receive_operation_id", "output_session_id"),
			v4TraceOptionalFields(
				v4TraceString,
				"receive_intent_digest", "certification", "root_disposition", "checkpoint_decision",
			),
			v4TraceObjectField("capabilities", filesystemCapabilities, true),
			v4TraceObjectField("native_lock", filesystemNativeLock, true),
			v4TraceObjectField("runtime_decision", filesystemRuntimeDecision, true),
			v4TraceObjectField("output_correlation", filesystemCorrelation, true),
			v4TraceObjectField("counters", filesystemCounters, false),
			v4TraceObjectField("failure", filesystemFailure, true),
		),
		"sender_terminal_send_observed": v4TraceSchema(
			v4TraceFields(v4TraceBool, "settled"),
			v4TraceFields(v4TraceString, "transport_disposition", "outcome", "decision"),
		),
		"sender_session_terminated": v4TraceSchema(v4TraceFields(v4TraceString, "trigger", "provenance")),
		"catalog_storage": v4TraceSchema(
			v4TraceFields(v4TraceString, "operation", "cause"),
			v4TraceObjectField("usage", catalogUsage, false),
			v4TraceFields(v4TraceDecimal, "legacy_roots_removed"),
		),
		"root_prefetch": v4TraceSchema(
			v4TraceFields(v4TraceString, "decision"),
			v4TraceFields(v4TraceDecimal, "attempt", "entry_count", "omitted_count"),
		),
		"protocol_operation": v4TraceSchema(
			v4TraceFields(v4TraceTimestamp, "observed_at"),
			v4TraceFields(v4TraceString, "role", "stage", "request_kind", "cause"),
			v4TraceOptionalFields(v4TraceString, "response_kind"),
			v4TraceObjectField("send", protocolSend, true),
			v4TraceFields(v4TraceDecimal, "response_count", "operation_elapsed_ms"),
			v4TraceOptionalFields(v4TraceDecimal, "deadline_remaining_ms"),
			v4TraceFields(v4TraceInteger, "usable_lanes_at_selection", "usable_lanes_at_settlement"),
		),
		"protocol_response_send_not_started": notStartedResponse,
		"protocol_response_send_returned":    response,
		"protocol_send_attempt_settled":      v4TraceSchema(v4TraceFields(v4TraceTimestamp, "observed_at"), v4TraceFields(v4TraceString, "role", "request_kind", "response_kind"), v4TraceFields(v4TraceDecimal, "response_sequence"), v4TraceObjectField("attempt", attempt, false)),
		"protocol_error_received":            v4TraceSchema(v4TraceFields(v4TraceTimestamp, "observed_at"), v4TraceFields(v4TraceString, "role", "request_kind"), v4TraceObjectField("protocol_error", protocolError, false)),
		"sender_content_decision":            v4TraceSchema(v4TraceFields(v4TraceTimestamp, "observed_at"), v4TraceFields(v4TraceString, "role", "request_kind"), v4TraceObjectField("content_decision", senderContentDecision, false)),
		"lane_settlement": v4TraceSchema(
			v4TraceFields(v4TraceString, "route"),
			v4TraceFields(v4TraceDecimal, "delivered_blocks", "delivered_bytes", "failed_block_attempts", "reassigned_blocks"),
			v4TraceFields(v4TraceBool, "incomplete"),
		),
		"observer_loss": v4TraceSchema(
			v4TraceFields(v4TraceString, "category", "reason"),
			v4TraceFields(v4TraceDecimal, "count", "omitted_samples"),
			v4TraceObjectField("rejection", rejection, true),
		),
		"receiver_termination": v4TraceSchema(
			v4TraceOptionalFields(v4TraceIdentity, "protocol_operation_id"),
			v4TraceFields(v4TraceDecimal, "local_generation"),
			v4TraceFields(
				v4TraceString,
				"transition_authority", "disposition", "transition_provenance",
				"consequence_provenance", "local_stop_reason",
			),
			v4TraceFields(v4TraceBool, "diagnostics_truncated", "peer_shutdown_failed", "channel_drain_failed"),
			v4TraceFields(v4TraceStringSlice, "benign_components", "retained_cause_classes", "teardown_transitions"),
		),
		"trace_summary": v4TraceSchema(
			v4TraceFields(v4TraceBool, "incomplete", "writer_failed", "flush_failed", "schema_limited"),
			v4TraceFields(v4TraceDecimal, "lifecycle_dropped", "progress_dropped", "events_written", "rejection_evidence_dropped"),
		),
	}
}

func v4TraceObjectSliceField(name string, object *v4TraceObjectSchema) []v4TraceFieldSchema {
	return []v4TraceFieldSchema{{name: name, kind: v4TraceObjectSlice, object: object}}
}
