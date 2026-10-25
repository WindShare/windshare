package contentflow

import diag "github.com/windshare/windshare/core/diagnostics"

func (value SenderDecisionTrace) DiagnosticValue() diag.Value {
	names := [...]string{"", "capacity_busy", "lease_relinquished", "lease_undelivered", "lease_detached",
		"block_lease_released", "block_lease_not_owned", "block_lease_expired", "block_lease_invalid"}
	stage := "unknown"
	if int(value.Stage) < len(names) {
		stage = names[value.Stage]
	}
	fields := diag.Fields{"kind": diag.Text(stage)}
	if value.CapacityDecisionID != "" {
		fields["capacity_decision_id"] = diag.Text(string(value.CapacityDecisionID))
	}
	if !value.LeaseID.IsZero() {
		fields["lease_id"] = diag.Hex(value.LeaseID.Bytes())
	}
	return diag.Object(fields)
}
