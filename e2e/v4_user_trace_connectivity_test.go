package e2e

func v4DownloadConnectivitySchema() *v4TraceObjectSchema {
	return v4TraceSchema(
		v4TraceFields(v4TraceHexIdentity, "download_id"),
		v4TraceFields(v4TraceDecimal, "direct_bytes", "turn_bytes", "application_relay_bytes", "unknown_bytes", "fallback_stall_ms"),
		v4TraceFields(v4TraceBool, "incomplete", "final"),
		[]v4TraceFieldSchema{
			{name: "first_direct_elapsed_ms", kind: v4TraceDecimal, nullable: true},
			{name: "direct_fraction", kind: v4TraceFraction, nullable: true},
		},
	)
}

func v4NativeConnectivitySchema() *v4TraceObjectSchema {
	candidate := v4TraceSchema(
		v4TraceFields(v4TraceString, "type", "protocol", "address", "family", "origin",
			"interface_class", "stun_endpoint", "stun_rtt_ms", "policy_decision"),
		v4TraceFields(v4TraceInteger, "port", "priority"),
		[]v4TraceFieldSchema{{name: "tcp_type", kind: v4TraceString, optional: true}},
	)
	pair := v4TraceSchema(
		v4TraceFields(v4TraceString, "local_type", "remote_type", "protocol", "local_address", "remote_address",
			"local_family", "remote_family", "pair_rtt_ms", "lifetime_ms", "switch_reason"),
		v4TraceFields(v4TraceInteger, "local_port", "remote_port"),
	)
	reachability := v4TraceSchema(
		v4TraceFields(v4TraceString, "local_endpoint", "remote_scope", "protocol", "reason"),
		v4TraceFields(v4TraceInteger, "server_epoch"),
		v4TraceFields(v4TraceBool, "server_restarted"),
	)
	lifecycle := v4TraceSchema(
		v4TraceFields(v4TraceBool, "content_demand", "direct_demand"),
		v4TraceFields(v4TraceString, "previous_network_generation_id"),
	)
	admission := v4TraceSchema(
		v4TraceFields(v4TraceDecimal, "active", "queued"),
		v4TraceFields(v4TraceString, "wait_ms", "starts_remaining", "stun_remaining", "active_time_remaining_ms"),
	)
	socket := v4TraceSchema(
		v4TraceFields(v4TraceString, "local_endpoint", "stun_server", "duration_ms", "result"),
	)
	// Provider-unavailable facts remain explicit "unknown" strings; the exported
	// envelope and every nested record still reject additional fields.
	return v4TraceSchema(
		v4TraceFields(v4TraceString, "kind", "state", "side", "attempt_sequence",
			"network_generation_id", "ice_profile_id", "observed_at"),
		v4TraceObjectField("candidate", candidate, true),
		v4TraceObjectField("selected_pair", pair, true),
		v4TraceObjectField("reachability", reachability, true),
		v4TraceObjectField("lifecycle", lifecycle, true),
		v4TraceObjectField("admission", admission, true),
		v4TraceObjectField("socket", socket, true),
	)
}
