package receiversession

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/catalogflow"
)

const (
	// MaxPrefetchedDirectoryBytes bounds the consume-once catalog pages retained
	// between ordinary output shape resolution and transfer job discovery so
	// large directories still stream without inflating memory.
	MaxPrefetchedDirectoryBytes uint64 = 1 << 20
	maxPrefetchedDirectories           = 16
)

// RecordPrefetchedDirectory retains a complete, authenticated terminal directory
// generation for one subsequent OpenDirectoryPages consumer on this live runtime.
func (runtime *Runtime) RecordPrefetchedDirectory(
	directory catalog.DirectoryID,
	pages []catalog.CatalogPage,
) {
	if runtime == nil || runtime.core == nil || runtime.Lifetime().Err() != nil ||
		directory.IsZero() || len(pages) == 0 {
		return
	}
	if !pages[len(pages)-1].Terminal() {
		return
	}
	totalBytes := prefetchedPagesBytes(pages)
	if totalBytes == 0 || totalBytes > MaxPrefetchedDirectoryBytes {
		return
	}
	runtime.prefetchMu.Lock()
	defer runtime.prefetchMu.Unlock()
	if runtime.prefetchClosed || runtime.Lifetime().Err() != nil {
		return
	}
	if existing, exists := runtime.prefetchedDirs[directory]; exists {
		runtime.prefetchedBytes -= prefetchedPagesBytes(existing)
		delete(runtime.prefetchedDirs, directory)
	}
	if len(runtime.prefetchedDirs) >= maxPrefetchedDirectories ||
		totalBytes > MaxPrefetchedDirectoryBytes-runtime.prefetchedBytes {
		return
	}
	if runtime.prefetchedDirs == nil {
		runtime.prefetchedDirs = make(map[catalog.DirectoryID][]catalog.CatalogPage, 2)
	}
	runtime.prefetchedDirs[directory] = append([]catalog.CatalogPage(nil), pages...)
	runtime.prefetchedBytes += totalBytes
}

func (runtime *Runtime) takePrefetchedDirectory(directory catalog.DirectoryID) ([]catalog.CatalogPage, bool) {
	if runtime == nil || directory.IsZero() {
		return nil, false
	}
	runtime.prefetchMu.Lock()
	defer runtime.prefetchMu.Unlock()
	if runtime.prefetchClosed || len(runtime.prefetchedDirs) == 0 {
		return nil, false
	}
	pages, ok := runtime.prefetchedDirs[directory]
	if !ok {
		return nil, false
	}
	delete(runtime.prefetchedDirs, directory)
	runtime.prefetchedBytes -= prefetchedPagesBytes(pages)
	return pages, true
}

func (runtime *Runtime) clearPrefetchedDirectories() {
	if runtime == nil {
		return
	}
	runtime.prefetchMu.Lock()
	runtime.prefetchClosed = true
	runtime.prefetchedDirs = nil
	runtime.prefetchedBytes = 0
	runtime.prefetchMu.Unlock()
}

func prefetchedPagesBytes(pages []catalog.CatalogPage) uint64 {
	var total uint64
	for _, page := range pages {
		total += page.EstimatedMemoryBytes()
	}
	return total
}

type prefetchRecordingCatalog struct {
	dependencies receiverTransferDependencies
}

func (source prefetchRecordingCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	cursor, err := source.dependencies.OpenDirectoryPagesLive(ctx, directory)
	if err != nil {
		return nil, err
	}
	return &prefetchRecordingCursor{
		runtime:   source.dependencies.runtime,
		directory: directory,
		cursor:    cursor,
	}, nil
}

type prefetchRecordingCursor struct {
	runtime   *Runtime
	directory catalog.DirectoryID
	cursor    catalog.DirectoryPageCursor
	pages     []catalog.CatalogPage
	bytes     uint64
	aborted   bool
}

func (cursor *prefetchRecordingCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	page, ok, err := cursor.cursor.Next(ctx)
	if err != nil {
		cursor.aborted = true
		cursor.pages = nil
		return page, ok, err
	}
	if !ok {
		// Only cache once the caller drains past the terminal page cleanly;
		// budget fallbacks or validation errors stop before this EOF step.
		if !cursor.aborted && len(cursor.pages) > 0 && cursor.pages[len(cursor.pages)-1].Terminal() {
			cursor.runtime.RecordPrefetchedDirectory(cursor.directory, cursor.pages)
		}
		cursor.aborted = true
		cursor.pages = nil
		return catalog.CatalogPage{}, false, nil
	}
	if !cursor.aborted {
		pageBytes := page.EstimatedMemoryBytes()
		if pageBytes == 0 || pageBytes > MaxPrefetchedDirectoryBytes-cursor.bytes {
			cursor.aborted = true
			cursor.pages = nil
		} else {
			cursor.pages = append(cursor.pages, page)
			cursor.bytes += pageBytes
		}
	}
	return page, true, nil
}

func (cursor *prefetchRecordingCursor) Close() error {
	cursor.aborted = true
	cursor.pages = nil
	return cursor.cursor.Close()
}

type prefetchedDirectoryPageCursor struct {
	pages        []catalog.CatalogPage
	index        int
	closed       bool
	dependencies receiverTransferDependencies
}

func (cursor *prefetchedDirectoryPageCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return catalog.CatalogPage{}, false, ctx.Err()
	}
	if cursor.dependencies.runtime == nil || cursor.dependencies.runtime.core == nil ||
		cursor.dependencies.runtime.Lifetime().Err() != nil {
		return catalog.CatalogPage{}, false, cursor.dependencies.classifyCatalog(ctx, session.ErrRuntimeClosed)
	}
	if cursor.closed {
		return catalog.CatalogPage{}, false, cursor.dependencies.classifyCatalog(ctx, catalogflow.ErrClientClosed)
	}
	if cursor.index >= len(cursor.pages) {
		return catalog.CatalogPage{}, false, nil
	}
	page := cursor.pages[cursor.index]
	cursor.index++
	return page, true, nil
}

func (cursor *prefetchedDirectoryPageCursor) Close() error {
	cursor.closed = true
	cursor.pages = nil
	return nil
}
