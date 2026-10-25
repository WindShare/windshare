package content

import (
	"fmt"

	diag "github.com/windshare/windshare/core/diagnostics"
)

const revisionEventName = "sender_revision"

func (v RevisionTrace) DiagnosticRecord() diag.Record {
	// Use the coordinator's canonical tuple so capacity and revision records join.
	revision := fmt.Sprintf("%x:%x", v.FileID().Bytes(), v.FileRevision().Bytes())
	f := diag.Fields{"stage": diag.Text(v.Stage().DiagnosticName()), "cause": diag.Text(v.Cause().DiagnosticName()),
		"revision_id": diag.Text(revision), "share_instance": diag.Identity(v.ShareInstance().Bytes())}
	if !v.LeaseID().IsZero() {
		f["lease_id"] = diag.Hex(v.LeaseID().Bytes())
	}
	r := diag.New(revisionEventName, f)
	if v.SessionID() != "" {
		r = r.Correlate(diag.Fields{"protocol_session_id": diag.Text(string(v.SessionID()))})
	}
	return r
}
