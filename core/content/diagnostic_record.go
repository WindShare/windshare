package content

import (
	"fmt"

	diag "github.com/windshare/windshare/core/diagnostics"
)

const (
	revisionEventName = "sender_revision"
	capacityEventName = "sender_capacity"
)

func (v RevisionTrace) DiagnosticRecord() diag.Record {
	revision := fmt.Sprintf("%x:%x", v.FileID().Bytes(), v.FileRevision().Bytes())
	f := diag.Fields{
		"stage":          diag.Text(v.Stage().DiagnosticName()),
		"cause":          diag.Text(v.Cause().DiagnosticName()),
		"revision_id":    diag.Text(revision),
		"share_instance": diag.Identity(v.ShareInstance().Bytes()),
	}
	if !v.LeaseID().IsZero() {
		f["lease_id"] = diag.Hex(v.LeaseID().Bytes())
	}
	r := diag.New(revisionEventName, f)
	if v.SessionID() != "" {
		r = r.Correlate(diag.Fields{"protocol_session_id": diag.Text(string(v.SessionID()))})
	}
	return r
}

func (v CapacityTraceEvent) DiagnosticRecord() diag.Record {
	f := diag.Fields{
		"stage":    diag.Text(v.Stage().DiagnosticName()),
		"share_id": diag.Text(v.ShareID()),
		"process":  capacityDiagnosticScope(v.snapshot.Process()),
	}
	if v.DecisionID() != "" {
		f["decision_id"] = diag.Text(string(v.DecisionID()))
	}
	if v.RevisionID() != "" {
		f["revision_id"] = diag.Text(v.RevisionID())
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
	return diag.Object(diag.Fields{
		"stable_handles":             diag.Uint(v.Used().StableHandles),
		"active_leases":              diag.Uint(v.Used().ActiveLeases),
		"stable_handle_limit":        diag.Uint(v.Limits().StableHandles),
		"active_lease_limit":         diag.Uint(v.Limits().ActiveLeases),
		"reclaimable_stable_handles": diag.Uint(v.ReclaimableStableHandles()),
	})
}
