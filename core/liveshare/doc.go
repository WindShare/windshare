// Package liveshare composes network-free suite-02 catalog, content, and session
// runtimes. Native application orchestration belongs to the engine package.
//
// Sender preparation consumes a FileSourceFactory and acquires only the selected
// roots' metadata. Source references remain private to the adapter; descendants
// and content are read on demand. The sender joins catalog and content users
// before closing its source so platform handles and share-use grants remain
// valid for the full lifetime of every dependent operation.
//
// Callers supply aggregate catalog, content-cache, and revision capacity budgets
// so simultaneous shares account against one application owner's limits.
package liveshare
