// Package osfs provides metadata-only source snapshots and root-confined
// receive sinks (execution plan §6.13).
//
// Sender traversal is anchored to an os.Root and never follows pre-existing
// symbolic links or Windows reparse points. Source reads on demand only after
// the opened file's identity and metadata match the snapshot, then verifies the
// same handle again after each read. Sink retains a separate os.Root capability,
// validates every manifest path, enforces platform path limits, and grants
// reopen authority only through an exact canonical-path OwnershipLedger.
// Collection-level collision validation remains in manifest's PathPolicy.
package osfs
