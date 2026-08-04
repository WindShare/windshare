package catalog

import "context"

// DirectoryPageCursor owns one forward-only authenticated generation walk.
// Keeping the cursor contract in catalog lets transports stream pages without
// forcing consumers to retain a DirectorySnapshot or depend on a wire adapter.
type DirectoryPageCursor interface {
	Next(context.Context) (CatalogPage, bool, error)
	Close() error
}
