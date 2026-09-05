package e2e

func v3DownloadConnectivitySchema() *v3TraceObjectSchema {
	return v3TraceSchema(
		v3TraceFields(v3TraceHexIdentity, "download_id"),
		v3TraceFields(v3TraceDecimal, "direct_bytes", "turn_bytes", "application_relay_bytes", "unknown_bytes", "fallback_stall_ms"),
		v3TraceFields(v3TraceBool, "incomplete", "final"),
		[]v3TraceFieldSchema{
			{name: "first_direct_elapsed_ms", kind: v3TraceDecimal, nullable: true},
			{name: "direct_fraction", kind: v3TraceFraction, nullable: true},
		},
	)
}

func v3NativeConnectivitySchema() *v3TraceObjectSchema {
	candidate := v3TraceSchema(
		v3TraceFields(v3TraceString, "type", "protocol", "address", "family", "origin",
			"interface_class", "stun_endpoint", "stun_rtt_ms", "policy_decision"),
		v3TraceFields(v3TraceInteger, "port"),
	)
	pair := v3TraceSchema(
		v3TraceFields(v3TraceString, "local_type", "remote_type", "protocol", "local_address", "remote_address",
			"local_family", "remote_family", "pair_rtt_ms", "lifetime_ms", "switch_reason"),
		v3TraceFields(v3TraceInteger, "local_port", "remote_port"),
	)
	reachability := v3TraceSchema(
		v3TraceFields(v3TraceString, "local_endpoint", "remote_scope", "protocol", "reason"),
		v3TraceFields(v3TraceInteger, "server_epoch"),
		v3TraceFields(v3TraceBool, "server_restarted"),
	)
	lifecycle := v3TraceSchema(
		v3TraceFields(v3TraceBool, "content_demand", "direct_demand"),
		v3TraceFields(v3TraceString, "previous_network_generation_id"),
	)
	admission := v3TraceSchema(
		v3TraceFields(v3TraceDecimal, "active", "queued"),
		v3TraceFields(v3TraceString, "wait_ms", "starts_remaining", "stun_remaining", "active_time_remaining_ms"),
	)
	// Provider-unavailable facts remain explicit "unknown" strings; the exported
	// envelope and every nested record still reject additional fields.
	return v3TraceSchema(
		v3TraceFields(v3TraceString, "kind", "state", "side", "attempt_sequence",
			"network_generation_id", "ice_profile_id", "observed_at"),
		v3TraceObjectField("candidate", candidate, true),
		v3TraceObjectField("selected_pair", pair, true),
		v3TraceObjectField("reachability", reachability, true),
		v3TraceObjectField("lifecycle", lifecycle, true),
		v3TraceObjectField("admission", admission, true),
	)
}
