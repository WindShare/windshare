package liveshare

import (
	diag "github.com/windshare/windshare/core/diagnostics"
)

const catalogStorageEventName = "catalog_storage"
const rootPrefetchEventName = "root_prefetch"

func (v CatalogStorageTrace) DiagnosticRecord() diag.Record {
	u := v.RecoveredUsage
	return diag.New(catalogStorageEventName, diag.Fields{
		"operation": diag.Text(v.Operation.DiagnosticName()), "cause": diag.Text(v.Cause.DiagnosticName()),
		"share_instance": diag.Identity(v.ShareInstance[:]), "legacy_roots_removed": diag.Uint(v.LegacyRootsRemoved),
		"usage": diag.Object(diag.Fields{"active_scans": diag.Uint(u.ActiveScans), "scan_work": diag.Uint(u.ScanWork),
			"entries": diag.Uint(u.Entries), "memory_bytes": diag.Uint(u.MemoryBytes), "spill_bytes": diag.Uint(u.SpillBytes)})})
}
func (v RootPrefetchTrace) DiagnosticRecord() diag.Record {
	return diag.New(rootPrefetchEventName, diag.Fields{"decision": diag.Text(v.Decision.DiagnosticName()),
		"share_instance": diag.Identity(v.ShareInstance[:]), "attempt": diag.Uint(v.Attempt),
		"entry_count": diag.Uint(v.EntryCount), "omitted_count": diag.Uint(v.OmittedCount)})
}
