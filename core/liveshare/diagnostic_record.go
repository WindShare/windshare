package liveshare

import (
	diag "github.com/windshare/windshare/core/diagnostics"
)

const rootPrefetchEventName = "root_prefetch"

func (v RootPrefetchTrace) DiagnosticRecord() diag.Record {
	return diag.New(rootPrefetchEventName, diag.Fields{"decision": diag.Text(v.Decision.DiagnosticName()),
		"share_instance": diag.Identity(v.ShareInstance[:]), "attempt": diag.Uint(v.Attempt),
		"entry_count": diag.Uint(v.EntryCount), "omitted_count": diag.Uint(v.OmittedCount)})
}
