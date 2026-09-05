package e2e

import (
	"crypto/sha256"
	"encoding/json"
)

const (
	v3TraceSchemaVersion = 3
	v3IdentityBytes      = 16
	v3DigestBytes        = sha256.Size
)

type v3TraceValueKind uint8

const (
	v3TraceString v3TraceValueKind = iota + 1
	v3TraceIdentity
	v3TraceRelaySessionIdentity
	v3TraceDecimal
	v3TraceInteger
	v3TraceBool
	v3TraceStringSlice
	v3TraceObject
	v3TraceHexIdentity
	v3TraceHexDigest
	v3TraceCorrelationValue
	v3TraceProtocolSettlement
	v3TraceFraction
)

type v3TraceFieldSchema struct {
	name     string
	kind     v3TraceValueKind
	object   *v3TraceObjectSchema
	optional bool
	nullable bool
}

type v3TraceObjectSchema struct {
	fields map[string]v3TraceFieldSchema
}

type v3TraceCorrelation struct {
	ProtocolSessionID   string
	ProtocolOperationID string
	PeerPathID          string
	PeerAttemptID       string
	LaneID              *uint32
	LaneEpoch           *uint32
}

type v3TraceRecord struct {
	Event        string
	RuntimeRunID string
	Correlation  *v3TraceCorrelation
	Payload      map[string]json.RawMessage
}

var v3TracePayloadSchemas = buildV3TracePayloadSchemas()

func v3TraceSchema(groups ...[]v3TraceFieldSchema) *v3TraceObjectSchema {
	fields := make(map[string]v3TraceFieldSchema)
	for _, group := range groups {
		for _, field := range group {
			if _, duplicate := fields[field.name]; duplicate {
				panic("duplicate v3 trace schema field: " + field.name)
			}
			fields[field.name] = field
		}
	}
	return &v3TraceObjectSchema{fields: fields}
}

func v3TraceFields(kind v3TraceValueKind, names ...string) []v3TraceFieldSchema {
	fields := make([]v3TraceFieldSchema, 0, len(names))
	for _, name := range names {
		fields = append(fields, v3TraceFieldSchema{name: name, kind: kind})
	}
	return fields
}

func v3TraceOptionalFields(kind v3TraceValueKind, names ...string) []v3TraceFieldSchema {
	fields := v3TraceFields(kind, names...)
	for index := range fields {
		fields[index].optional = true
	}
	return fields
}

func v3TraceObjectField(name string, object *v3TraceObjectSchema, optional bool) []v3TraceFieldSchema {
	return []v3TraceFieldSchema{{name: name, kind: v3TraceObject, object: object, optional: optional}}
}

func buildV3TracePayloadSchemas() map[string]*v3TraceObjectSchema {
	relayAuthority := v3TraceSchema(
		v3TraceFields(v3TraceString, "scheme", "host"),
		v3TraceFields(v3TraceInteger, "port"),
	)
	fault := v3TraceSchema(
		v3TraceFields(v3TraceString, "domain", "scope"),
		v3TraceFields(v3TraceInteger, "code"),
	)
	failure := v3TraceSchema(
		v3TraceFields(v3TraceString, "code", "message_key"),
		v3TraceObjectField("fault", fault, true),
		v3TraceOptionalFields(v3TraceDecimal, "retry_after_ms"),
	)
	fileOutcomes := v3TraceSchema(v3TraceFields(
		v3TraceDecimal,
		"downloaded_files", "resumed_files", "paused_files", "collision_files",
		"item_blocked_files", "failed_files", "modified_time_warnings",
	))
	capacityWait := v3TraceSchema(v3TraceFields(
		v3TraceDecimal, "active_waiters", "accumulated_wait_ms", "attempts",
	))
	progress := v3TraceSchema(
		v3TraceFields(v3TraceString, "discovery"),
		v3TraceFields(v3TraceBool, "counters_exact"),
		v3TraceFields(
			v3TraceDecimal,
			"discovered_files", "discovered_bytes", "published_files", "published_bytes",
			"verified_bytes", "newly_verified_bytes",
		),
		v3TraceObjectField("file_outcomes", fileOutcomes, false),
		v3TraceObjectField("capacity_wait", capacityWait, false),
	)
	capacityScope := v3TraceSchema(v3TraceFields(
		v3TraceDecimal,
		"stable_handles", "active_leases", "stable_handle_limit", "active_lease_limit",
		"reclaimable_stable_handles", "quarantined_stable_handles", "pending_admissions", "active_reclaims",
	))
	transferCapacity := v3TraceSchema(
		v3TraceFields(v3TraceIdentity, "wait_id", "generation_id", "protocol_operation_id"),
		v3TraceFields(v3TraceDecimal, "attempt", "hint_ms", "jitter_ms", "delay_ms", "accumulated_wait_ms"),
		v3TraceFields(v3TraceInteger, "active_waiters"),
	)
	senderContentDecision := v3TraceSchema(
		v3TraceFields(v3TraceString, "kind"),
		v3TraceOptionalFields(v3TraceHexDigest, "capacity_decision_id"),
		v3TraceOptionalFields(v3TraceHexIdentity, "lease_id"),
	)
	peerPhaseDeadline := v3TraceSchema(
		v3TraceFields(v3TraceString, "phase"),
		v3TraceFields(v3TraceDecimal, "deadline_ms"),
	)
	peerCandidates := v3TraceSchema(v3TraceFields(v3TraceInteger, "local_emitted", "remote_accepted"))
	peerAdmission := v3TraceSchema(v3TraceFields(v3TraceString, "disposition", "response_delivery"))
	peerRejection := v3TraceSchema(
		v3TraceFields(v3TraceString, "code"),
		v3TraceOptionalFields(v3TraceDecimal, "retry_after_ms"),
	)
	peerFailure := v3TraceSchema(
		v3TraceFields(v3TraceString, "failed_at_stage", "scope"),
		v3TraceObjectField("failure", failure, false),
	)
	filesystemNativeLock := v3TraceSchema(v3TraceFields(v3TraceString, "scope", "milestone"))
	filesystemRuntimeDecision := v3TraceSchema(v3TraceFields(v3TraceString, "component", "operation", "decision"))
	filesystemCorrelation := v3TraceSchema(v3TraceOptionalFields(v3TraceDecimal, "operation_id", "claim_id"))
	filesystemCounters := v3TraceSchema(v3TraceFields(
		v3TraceDecimal,
		"node_claims", "directory_claims", "file_claims", "active_file_claims",
		"reserved_file_slots", "directory_metadata_bytes", "checkpoint_records",
	))
	filesystemFailure := v3TraceSchema(
		v3TraceFields(v3TraceString, "stage"),
		v3TraceOptionalFields(v3TraceString, "reconciliation_step", "native_error_class"),
		v3TraceObjectField("failure", failure, false),
	)
	catalogUsage := v3TraceSchema(v3TraceFields(
		v3TraceDecimal,
		"active_scans", "scan_work", "entries", "memory_bytes", "spill_bytes",
	))
	protocolSend := v3TraceSchema(
		v3TraceFields(v3TraceBool, "settled", "admitted"),
		v3TraceFields(v3TraceString, "outcome"),
	)
	protocolFailure := v3TraceSchema(
		v3TraceFields(v3TraceString, "request_kind", "wire_scope"),
		v3TraceFields(v3TraceInteger, "wire_code"),
		v3TraceFields(v3TraceBool, "retryable"),
		v3TraceOptionalFields(v3TraceInteger, "retry_after_ms"),
		v3TraceFields(v3TraceProtocolSettlement, "settlement"),
		v3TraceFields(v3TraceCorrelationValue, "correlation"),
	)

	return map[string]*v3TraceObjectSchema{
		"ready":               v3TraceSchema(),
		"platform_setup":      v3TraceSchema(v3TraceFields(v3TraceString, "state", "reason")),
		"native_connectivity": v3NativeConnectivitySchema(),
		"sharing_subject_selected": v3TraceSchema(
			v3TraceFields(v3TraceString, "subject_kind"),
			v3TraceFields(v3TraceDecimal, "selected_items"),
			v3TraceOptionalFields(v3TraceDecimal, "file_bytes"),
		),
		"relay_connected": v3TraceSchema(v3TraceObjectField("relay_authority", relayAuthority, false)),
		"relay_recovering": v3TraceSchema(
			v3TraceObjectField("relay_authority", relayAuthority, false),
			v3TraceFields(v3TraceInteger, "attempt"),
			v3TraceFields(v3TraceString, "state"),
			v3TraceObjectField("failure", failure, true),
		),
		"content_path_selected": v3TraceSchema(v3TraceFields(v3TraceString, "content_path")),
		"fallback": v3TraceSchema(
			v3TraceFields(v3TraceString, "from_transport", "to_transport"),
			v3TraceObjectField("failure", failure, false),
		),
		"transfer_progress": v3TraceSchema(
			v3TraceFields(v3TraceIdentity, "receive_operation_id", "transfer_job_id"),
			v3TraceObjectField("progress", progress, false),
		),
		"warning": v3TraceSchema(v3TraceObjectField("failure", failure, false)),
		"command_failed": v3TraceSchema(
			v3TraceFields(v3TraceInteger, "exit_code"),
			v3TraceObjectField("failure", failure, false),
		),
		"transfer_settled": v3TraceSchema(
			v3TraceObjectField("download_connectivity", v3DownloadConnectivitySchema(), true),
			v3TraceFields(v3TraceString, "result_status", "drift"),
			v3TraceFields(v3TraceInteger, "exit_code"),
			v3TraceFields(v3TraceDecimal, "result_elapsed_ms", "directory_failures", "omitted_diagnostics", "published_bytes"),
			v3TraceFields(v3TraceBool, "destination_adjusted", "counters_exact"),
			v3TraceObjectField("file_outcomes", fileOutcomes, false),
			v3TraceObjectField("failure", failure, true),
		),
		"sharing_stopped": v3TraceSchema(
			v3TraceFields(v3TraceInteger, "exit_code"),
			v3TraceFields(v3TraceDecimal, "result_elapsed_ms"),
			v3TraceFields(v3TraceBool, "stopped_cleanly"),
			v3TraceObjectField("failure", failure, true),
		),
		"trace_incomplete": v3TraceSchema(
			v3TraceFields(v3TraceString, "cause"),
			v3TraceFields(v3TraceDecimal, "lifecycle_dropped", "progress_dropped"),
		),
		"lane_adopted": v3TraceSchema(v3TraceFields(v3TraceString, "transport")),
		"relay_lifecycle": v3TraceSchema(
			v3TraceFields(v3TraceDecimal, "link_id"),
			v3TraceOptionalFields(v3TraceRelaySessionIdentity, "relay_session_id"),
			v3TraceOptionalFields(v3TraceDecimal, "send_operation_id", "dropped"),
			v3TraceFields(v3TraceString, "stage", "retirement_source", "cause", "drain_cause"),
			v3TraceOptionalFields(v3TraceString, "disposition"),
			v3TraceFields(v3TraceBool, "terminal"),
		),
		"webrtc_lifecycle": v3TraceSchema(
			v3TraceFields(v3TraceDecimal, "channel_id"),
			v3TraceOptionalFields(v3TraceDecimal, "send_operation_id", "dropped"),
			v3TraceFields(v3TraceString, "operation", "transition", "state", "terminal_state", "cause"),
			v3TraceOptionalFields(v3TraceString, "disposition"),
		),
		"peer_attempt": v3TraceSchema(
			v3TraceFields(v3TraceDecimal, "attempt_sequence", "attempt_elapsed_ms"),
			v3TraceFields(v3TraceString, "stage"),
			v3TraceOptionalFields(v3TraceIdentity, "offer_operation_id", "grant_operation_id"),
			v3TraceObjectField("phase_deadline", peerPhaseDeadline, true),
			v3TraceObjectField("candidates", peerCandidates, true),
			v3TraceObjectField("admission", peerAdmission, true),
			v3TraceObjectField("rejection", peerRejection, true),
			v3TraceObjectField("failure", peerFailure, true),
		),
		"transfer_lifecycle": v3TraceSchema(
			v3TraceFields(v3TraceIdentity, "receive_operation_id", "transfer_job_id"),
			v3TraceFields(v3TraceString, "stage", "file_selection", "file_settlement", "tree_settlement"),
			v3TraceOptionalFields(v3TraceString, "item_block_reason"),
			v3TraceObjectField("progress", progress, false),
			v3TraceObjectField("capacity", transferCapacity, true),
			v3TraceObjectField("failure", failure, true),
		),
		"sender_capacity": v3TraceSchema(
			v3TraceFields(v3TraceString, "stage"),
			v3TraceOptionalFields(v3TraceHexDigest, "decision_id", "revision_id"),
			v3TraceObjectField("process", capacityScope, false),
			v3TraceObjectField("share", capacityScope, true),
			v3TraceObjectField("session", capacityScope, true),
		),
		"sender_revision": v3TraceSchema(
			v3TraceFields(v3TraceString, "stage", "cause"),
			v3TraceFields(v3TraceHexDigest, "revision_id"),
			v3TraceOptionalFields(v3TraceHexIdentity, "lease_id"),
		),
		"filesystem_output": v3TraceSchema(
			v3TraceFields(v3TraceString, "operation"),
			v3TraceOptionalFields(v3TraceIdentity, "receive_operation_id", "output_session_id"),
			v3TraceOptionalFields(
				v3TraceString,
				"receive_intent_digest", "certification", "root_disposition", "checkpoint_decision",
			),
			v3TraceObjectField("native_lock", filesystemNativeLock, true),
			v3TraceObjectField("runtime_decision", filesystemRuntimeDecision, true),
			v3TraceObjectField("output_correlation", filesystemCorrelation, true),
			v3TraceObjectField("counters", filesystemCounters, false),
			v3TraceObjectField("failure", filesystemFailure, true),
		),
		"sender_terminal_send_observed": v3TraceSchema(
			v3TraceFields(v3TraceBool, "settled"),
			v3TraceFields(v3TraceString, "transport_disposition", "outcome", "decision"),
		),
		"sender_session_terminated": v3TraceSchema(v3TraceFields(v3TraceString, "trigger", "provenance")),
		"catalog_storage": v3TraceSchema(
			v3TraceFields(v3TraceString, "operation", "cause"),
			v3TraceObjectField("usage", catalogUsage, false),
			v3TraceFields(v3TraceDecimal, "legacy_roots_removed"),
		),
		"root_prefetch": v3TraceSchema(
			v3TraceFields(v3TraceString, "decision"),
			v3TraceFields(v3TraceDecimal, "attempt", "entry_count", "omitted_count"),
		),
		"protocol_operation": v3TraceSchema(
			v3TraceFields(v3TraceString, "role", "stage", "request_kind", "cause"),
			v3TraceOptionalFields(v3TraceString, "response_kind"),
			v3TraceObjectField("send", protocolSend, true),
			v3TraceFields(v3TraceDecimal, "response_count", "operation_elapsed_ms"),
			v3TraceOptionalFields(v3TraceDecimal, "deadline_remaining_ms"),
			v3TraceFields(v3TraceInteger, "usable_lanes_at_selection", "usable_lanes_at_settlement"),
			v3TraceObjectField("protocol_failure", protocolFailure, true),
			v3TraceObjectField("content_decision", senderContentDecision, true),
		),
		"lane_settlement": v3TraceSchema(
			v3TraceFields(v3TraceString, "route"),
			v3TraceFields(v3TraceDecimal, "delivered_blocks", "delivered_bytes", "failed_block_attempts", "reassigned_blocks"),
			v3TraceFields(v3TraceBool, "incomplete"),
		),
		"observer_loss": v3TraceSchema(
			v3TraceFields(v3TraceString, "category", "reason"),
			v3TraceFields(v3TraceDecimal, "count"),
		),
		"receiver_termination": v3TraceSchema(
			v3TraceOptionalFields(v3TraceIdentity, "protocol_operation_id"),
			v3TraceFields(v3TraceDecimal, "local_generation"),
			v3TraceFields(
				v3TraceString,
				"transition_authority", "disposition", "transition_provenance",
				"consequence_provenance", "local_stop_reason",
			),
			v3TraceFields(v3TraceBool, "diagnostics_truncated", "peer_shutdown_failed", "channel_drain_failed"),
			v3TraceFields(v3TraceStringSlice, "benign_components", "retained_cause_classes", "teardown_transitions"),
		),
		"trace_summary": v3TraceSchema(
			v3TraceFields(v3TraceBool, "incomplete", "writer_failed", "flush_failed", "schema_limited"),
			v3TraceFields(v3TraceDecimal, "lifecycle_dropped", "progress_dropped", "events_written"),
		),
	}
}
