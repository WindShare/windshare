package catalog

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type failureDirectoryVisitor func(DirectoryID, string) error

func (b *FileCatalogBackend) visitFailureDirectoriesLocked(
	ctx context.Context,
	visit failureDirectoryVisitor,
) error {
	root, err := os.Open(b.failuresDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for {
		entries, readErr := root.ReadDir(failureDirectoryReadBatch)
		if err := visitFailureDirectoryBatch(ctx, b.failuresDir, entries, visit); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func visitFailureDirectoryBatch(
	ctx context.Context,
	root string,
	entries []os.DirEntry,
	visit failureDirectoryVisitor,
) error {
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, err := decodeFailurePathIdentity[DirectoryID](entry.Name(), entry.IsDir())
		if err != nil {
			return err
		}
		if err := visit(directory, filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

type emptyFailureDirectoryCleaner struct {
	removed bool
}

func (cleaner *emptyFailureDirectoryCleaner) visit(_ DirectoryID, path string) error {
	empty, err := failureDirectoryIsEmpty(path)
	if err != nil || !empty {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	cleaner.removed = true
	return nil
}

func failureDirectoryIsEmpty(path string) (bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	children, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, err
	}
	return len(children) == 0, nil
}

func (b *FileCatalogBackend) cleanEmptyFailureDirectoriesLocked(ctx context.Context) error {
	cleaner := &emptyFailureDirectoryCleaner{}
	if err := b.visitFailureDirectoriesLocked(ctx, cleaner.visit); err != nil {
		return err
	}
	if cleaner.removed {
		return syncCatalogDirectory(b.failuresDir)
	}
	return nil
}

type failureReplayWalker struct {
	backend *FileCatalogBackend
	ctx     context.Context
	yield   func(fileFailureMeta, bool) error
	usage   ResourceUsage
}

func (walker *failureReplayWalker) visit(directory DirectoryID, _ string) error {
	usage, err := walker.backend.replayFailureDirectoryLocked(walker.ctx, directory, walker.yield)
	if err != nil {
		return err
	}
	next, ok := addUsage(walker.usage, usage)
	if !ok {
		return ErrBudgetExceeded
	}
	walker.usage = next
	return nil
}

func (b *FileCatalogBackend) walkFailuresLocked(
	ctx context.Context,
	yield func(fileFailureMeta, bool) error,
) (ResourceUsage, error) {
	walker := &failureReplayWalker{backend: b, ctx: ctx, yield: yield}
	if err := b.visitFailureDirectoriesLocked(ctx, walker.visit); err != nil {
		return ResourceUsage{}, err
	}
	return walker.usage, nil
}

func (b *FileCatalogBackend) replayFailureDirectoryLocked(
	ctx context.Context,
	directory DirectoryID,
	yield func(fileFailureMeta, bool) error,
) (ResourceUsage, error) {
	records, err := b.failureRecordsLocked(directory)
	if err != nil {
		return ResourceUsage{}, err
	}
	tail, err := validateFailureChain(records)
	if err != nil {
		return ResourceUsage{}, err
	}
	var usage ResourceUsage
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return ResourceUsage{}, err
		}
		meta, found, err := b.loadFailureLocked(directory, record.attempt)
		if err != nil || !found {
			return ResourceUsage{}, ErrCorruptCatalogStorage
		}
		next, ok := addUsage(usage, ResourceUsage{
			MemoryBytes: ScanAttemptLedgerBytes, SpillBytes: meta.spillBytes,
		})
		if !ok {
			return ResourceUsage{}, ErrBudgetExceeded
		}
		usage = next
		if yield != nil {
			if err := yield(meta, record.attempt == tail); err != nil {
				return ResourceUsage{}, err
			}
		}
	}
	return usage, nil
}
