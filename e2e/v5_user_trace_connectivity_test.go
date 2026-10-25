package e2e

func v5DownloadConnectivitySchema() *v5TraceObjectSchema {
	return v5TraceSchema(
		v5TraceFields(v5TraceHexIdentity, "download_id"),
		v5TraceFields(v5TraceDecimal, "direct_bytes", "turn_bytes", "application_relay_bytes", "unknown_bytes", "fallback_stall_ms"),
		v5TraceFields(v5TraceBool, "incomplete", "final"),
		[]v5TraceFieldSchema{
			{name: "first_direct_elapsed_ms", kind: v5TraceDecimal, nullable: true},
			{name: "direct_fraction", kind: v5TraceFraction, nullable: true},
		},
	)
}
