package revisioncapacity

import (
	diag "github.com/windshare/windshare/core/diagnostics"
)

const capacityEventName = "sender_capacity"

func (v TraceEvent) DiagnosticRecord() diag.Record {
	f := diag.Fields{"stage": diag.Text(v.Stage().DiagnosticName()), "capacity_store_id": diag.Text(string(v.StoreID())),
		"capacity_share_id": diag.Text(string(v.ShareID())), "process": capacityDiagnosticScope(v.snapshot.Process())}
	if v.ClaimID() != "" {
		f["reclaim_claim_id"] = diag.Text(string(v.ClaimID()))
	}
	if v.CandidateToken() != "" {
		f["candidate_token"] = diag.Text(string(v.CandidateToken()))
	}
	if v.DecisionID() != "" {
		f["decision_id"] = diag.Text(string(v.DecisionID()))
	}
	if v.RevisionID() != "" {
		f["revision_id"] = diag.Text(string(v.RevisionID()))
	}
	if s := v.snapshot.Share(); s.Scope() != 0 {
		f["share"] = capacityDiagnosticScope(s)
	}
	for _, s := range v.snapshot.sessions {
		if s.Identity() == string(v.SessionID()) {
			f["session"] = capacityDiagnosticScope(s)
			break
		}
	}
	if v.diagnostic != nil {
		f["error"] = diag.Text(v.diagnostic.Error())
	}
	r := diag.New(capacityEventName, f)
	if v.SessionID() != "" {
		r = r.Correlate(diag.Fields{"protocol_session_id": diag.Text(string(v.SessionID()))})
	}
	return r
}
func capacityDiagnosticScope(v ScopeSnapshot) diag.Value {
	return diag.Object(diag.Fields{"stable_handles": diag.Uint(v.Used().StableHandles), "active_leases": diag.Uint(v.Used().ActiveLeases),
		"stable_handle_limit": diag.Uint(v.Limits().StableHandles), "active_lease_limit": diag.Uint(v.Limits().ActiveLeases),
		"reclaimable_stable_handles": diag.Uint(v.ReclaimableStableHandles()), "quarantined_stable_handles": diag.Uint(v.QuarantinedStableHandles()),
		"pending_admissions": diag.Uint(v.PendingAdmissions()), "active_reclaims": diag.Uint(v.ActiveReclaims())})
}
