package e2e

import (
	"crypto/sha256"
	"encoding/json"
)

const (
	v5TraceSchemaVersion = 5
	v5IdentityBytes      = 16
	v5DigestBytes        = sha256.Size
)

type v5TraceValueKind uint8

const (
	v5TraceString v5TraceValueKind = iota + 1
	v5TraceIdentity
	v5TraceRelaySessionIdentity
	v5TraceDecimal
	v5TraceInteger
	v5TraceBool
	v5TraceStringSlice
	v5TraceObject
	v5TraceHexIdentity
	v5TraceHexDigest
	v5TraceCorrelationValue
	v5TraceObjectSlice
	v5TraceTimestamp
	v5TraceRawString
	v5TraceFraction
	v5TraceRate
)

type v5TraceFieldSchema struct {
	name     string
	kind     v5TraceValueKind
	object   *v5TraceObjectSchema
	optional bool
	nullable bool
}

type v5TraceObjectSchema struct {
	fields map[string]v5TraceFieldSchema
}

type v5TraceCorrelation struct {
	ShareInstanceID     string
	ReceiveOperationID  string
	TransferJobID       string
	ProtocolSessionID   string
	ProtocolOperationID string
	PeerPathID          string
	PeerAttemptID       string
	LaneID              *uint32
	LaneEpoch           *uint32
}

type v5TraceRecord struct {
	Scope struct {
		EngineID string `json:"engine_id"`
		TaskID   string `json:"engine_task_id"`
		Workflow string `json:"workflow"`
	}
	Command      string
	Event        string
	RuntimeRunID string
	Correlation  *v5TraceCorrelation
	Payload      map[string]json.RawMessage
}

var v5TracePayloadSchemas = buildV5TracePayloadSchemas()

func v5TraceSchema(groups ...[]v5TraceFieldSchema) *v5TraceObjectSchema {
	fields := make(map[string]v5TraceFieldSchema)
	for _, group := range groups {
		for _, field := range group {
			if _, duplicate := fields[field.name]; duplicate {
				panic("duplicate v4 trace schema field: " + field.name)
			}
			fields[field.name] = field
		}
	}
	return &v5TraceObjectSchema{fields: fields}
}

func v5TraceFields(kind v5TraceValueKind, names ...string) []v5TraceFieldSchema {
	fields := make([]v5TraceFieldSchema, 0, len(names))
	for _, name := range names {
		fields = append(fields, v5TraceFieldSchema{name: name, kind: kind})
	}
	return fields
}

func v5TraceOptionalFields(kind v5TraceValueKind, names ...string) []v5TraceFieldSchema {
	fields := v5TraceFields(kind, names...)
	for index := range fields {
		fields[index].optional = true
	}
	return fields
}

func v5TraceObjectField(name string, object *v5TraceObjectSchema, optional bool) []v5TraceFieldSchema {
	return []v5TraceFieldSchema{{name: name, kind: v5TraceObject, object: object, optional: optional}}
}

func buildV5TracePayloadSchemas() map[string]*v5TraceObjectSchema {
	relayOrigin := v5TraceSchema(
		v5TraceFields(v5TraceString, "scheme", "host"),
		v5TraceFields(v5TraceInteger, "port"),
	)
	relayRecoveryDetails := v5TraceSchema(
		v5TraceOptionalFields(v5TraceIdentity, "share_instance"),
		v5TraceFields(v5TraceDecimal, "connection_generation", "next_delay_ms"),
		v5TraceFields(v5TraceBool, "slow_wait", "resume_registration", "terminal"),
	)
	fault := v5TraceSchema(
		v5TraceFields(v5TraceString, "domain", "scope"),
		v5TraceFields(v5TraceInteger, "code"),
	)
	failure := v5TraceSchema(
		v5TraceFields(v5TraceString, "code", "message_key"),
		v5TraceObjectField("fault", fault, true),
		v5TraceOptionalFields(v5TraceDecimal, "retry_after_ms"),
	)
	fileOutcomes := v5TraceSchema(v5TraceFields(
		v5TraceDecimal,
		"downloaded_files", "resumed_files", "previously_published_files", "paused_files", "collision_files",
		"item_blocked_files", "failed_files", "modified_time_warnings",
	))
	capacityWait := v5TraceSchema(v5TraceFields(
		v5TraceDecimal, "active_waiters", "accumulated_wait_ms", "attempts",
	))
	progress := v5TraceSchema(
		v5TraceFields(v5TraceString, "discovery"),
		v5TraceFields(v5TraceBool, "counters_exact"),
		v5TraceFields(
			v5TraceDecimal,
			"discovered_files", "discovered_bytes", "published_files", "published_bytes",
			"verified_bytes", "newly_verified_bytes", "previously_published_bytes", "restart_reusable_bytes",
		),
		v5TraceObjectField("file_outcomes", fileOutcomes, false),
		v5TraceObjectField("capacity_wait", capacityWait, false),
	)
	rejectionField := v5TraceSchema(v5TraceFields(v5TraceString, "field", "representation"), v5TraceFields(v5TraceRawString, "value"))
	rejection := v5TraceSchema(v5TraceFields(v5TraceString, "source_event", "source_location", "source_stage", "field", "rule"), v5TraceOptionalFields(v5TraceRawString, "sample_protocol_session_id", "sample_protocol_operation_id", "sample_revision_id", "sample_response_sequence", "sample_attempt_sequence"), v5TraceObjectSliceField("evidence", rejectionField), v5TraceFields(v5TraceBool, "truncated"), v5TraceFields(v5TraceDecimal, "omitted_fields", "omitted_bytes"))

	return map[string]*v5TraceObjectSchema{
		"engine_task_observed": v5TraceSchema(
			v5TraceFields(v5TraceString, "engine_task_id", "stage", "stop_reason"),
			v5TraceOptionalFields(v5TraceString, "outcome", "failure_class"),
			v5TraceFields(v5TraceTimestamp, "observed_at"),
			v5TraceObjectField("failure", failure, true),
			v5TraceObjectField("cleanup_failure", failure, true),
		),
		"ready":          v5TraceSchema(),
		"platform_setup": v5TraceSchema(v5TraceFields(v5TraceString, "state", "reason")),
		"sharing_subject_selected": v5TraceSchema(
			v5TraceFields(v5TraceString, "subject_kind"),
			v5TraceFields(v5TraceDecimal, "selected_items"),
			v5TraceOptionalFields(v5TraceDecimal, "file_bytes"),
		),
		"relay_connected": v5TraceSchema(v5TraceObjectField("relay_origin", relayOrigin, false)),
		"relay_availability": v5TraceSchema(
			v5TraceFields(v5TraceInteger, "available", "total", "terminal"),
			v5TraceFields(v5TraceBool, "ever_ready"),
		),
		"relay_recovering": v5TraceSchema(
			v5TraceObjectField("relay_origin", relayOrigin, false),
			v5TraceFields(v5TraceInteger, "attempt"),
			v5TraceFields(v5TraceString, "state"),
			v5TraceObjectField("failure", failure, true),
			v5TraceObjectField("details", relayRecoveryDetails, true),
		),
		"content_path_selected": v5TraceSchema(v5TraceFields(v5TraceString, "content_path")),
		"fallback": v5TraceSchema(
			v5TraceFields(v5TraceString, "from_transport", "to_transport"),
			v5TraceObjectField("failure", failure, false),
		),
		"transfer_progress": v5TraceSchema(
			v5TraceFields(v5TraceIdentity, "receive_operation_id", "transfer_job_id"),
			v5TraceObjectField("progress", progress, false),
		),
		"warning": v5TraceSchema(v5TraceObjectField("failure", failure, false)),
		"command_failed": v5TraceSchema(
			v5TraceFields(v5TraceInteger, "exit_code"),
			v5TraceObjectField("failure", failure, false),
		),
		"transfer_settled": v5TraceSchema(
			v5TraceObjectField("download_connectivity", v5DownloadConnectivitySchema(), true),
			v5TraceFields(v5TraceString, "result_status", "drift"),
			v5TraceFields(v5TraceInteger, "exit_code"),
			v5TraceFields(v5TraceDecimal, "result_elapsed_ms", "directory_failures", "omitted_diagnostics", "published_bytes"),
			v5TraceFields(v5TraceBool, "destination_adjusted", "counters_exact"),
			v5TraceObjectField("file_outcomes", fileOutcomes, false),
			v5TraceObjectField("failure", failure, true),
		),
		"sharing_stopped": v5TraceSchema(
			v5TraceFields(v5TraceInteger, "exit_code"),
			v5TraceFields(v5TraceDecimal, "result_elapsed_ms"),
			v5TraceFields(v5TraceBool, "stopped_cleanly"),
			v5TraceObjectField("failure", failure, true),
		),
		"trace_incomplete": v5TraceSchema(
			v5TraceFields(v5TraceString, "cause"),
			v5TraceFields(v5TraceDecimal, "contract_rejected", "lifecycle_dropped", "progress_dropped"),
		),
		"lane_adopted": v5TraceSchema(v5TraceFields(v5TraceString, "transport")),
		"observer_loss": v5TraceSchema(
			v5TraceFields(v5TraceString, "category", "reason"),
			v5TraceFields(v5TraceDecimal, "count", "omitted_samples"),
			v5TraceObjectField("rejection", rejection, true),
		),
		"trace_summary": v5TraceSchema(
			v5TraceFields(v5TraceBool, "incomplete", "writer_failed", "flush_failed", "schema_limited"),
			v5TraceFields(v5TraceDecimal, "contract_rejected", "lifecycle_dropped", "progress_dropped", "events_written", "rejection_evidence_dropped"),
		),
	}
}

func v5TraceObjectSliceField(name string, object *v5TraceObjectSchema) []v5TraceFieldSchema {
	return []v5TraceFieldSchema{{name: name, kind: v5TraceObjectSlice, object: object}}
}
