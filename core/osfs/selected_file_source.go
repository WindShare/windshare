package osfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
)

// SelectedPathError retains the caller's selection at the point acquisition
// fails. A native error may name a parent directory instead; consumers need the
// selected path to identify which item in a multi-root share needs attention.
type SelectedPathError struct {
	Path  string
	Cause error
}

func (err *SelectedPathError) Error() string {
	return fmt.Sprintf("select source %s: %v", pathfailure.Quote(err.Path), err.Cause)
}

func (err *SelectedPathError) Unwrap() error { return err.Cause }

// SelectedFileSource owns all filesystem authority for one selection. In
// particular, selecting a file borrows its parent directory handle only for
// lookup; that handle does not authorize sharing its siblings.
type SelectedFileSource struct {
	selected  *SelectedCatalogSource
	revisions *RootedRevisionSource
	roots     []catalog.NodeRecord
	closeOnce sync.Once
	closeErr  error
}

func NewSelectedFileSource(ctx context.Context, config SelectedCatalogSourceConfig) (*SelectedFileSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	selected, err := newSelectedCatalogSource(ctx, config)
	if err != nil {
		return nil, err
	}
	source := &SelectedFileSource{selected: selected, roots: selected.SelectedRoots()}
	revisions, err := selected.RevisionSource()
	if err != nil {
		return nil, errors.Join(classifySourceError("acquire stable reads", catalog.SourceReference{}, err), source.Close())
	}
	source.revisions = revisions
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

func (source *SelectedFileSource) SelectedRoots() []catalog.NodeRecord {
	return append([]catalog.NodeRecord(nil), source.roots...)
}

func (source *SelectedFileSource) authorize(record catalog.NodeRecord) error {
	location, err := parseSourceReference(record.SourceReference())
	if err != nil || int(location.RootSlot()) >= len(source.roots) {
		return content.ErrRevisionStale
	}
	root := source.roots[location.RootSlot()]
	if root.Kind() == catalog.NodeKindFile && (record.NodeID() != root.NodeID() || record.SourceReference() != root.SourceReference()) {
		return content.ErrRevisionStale
	}
	return nil
}

func (source *SelectedFileSource) ScanDirectory(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
	if err := source.authorize(request.Directory); err != nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(classifySourceError("scan directory", request.Directory.SourceReference(), err))
	}
	result, err := source.selected.ScanDirectory(ctx, request)
	return result, classifySourceError("scan directory", request.Directory.SourceReference(), err)
}

func (source *SelectedFileSource) RevisionContinuity(record catalog.NodeRecord) (content.RevisionContinuity, error) {
	if err := source.authorize(record); err != nil {
		return 0, classifySourceError("select revision continuity", record.SourceReference(), err)
	}
	continuity, err := source.revisions.RevisionContinuity(record)
	return continuity, classifySourceError("select revision continuity", record.SourceReference(), err)
}

func (source *SelectedFileSource) OpenStable(ctx context.Context, record catalog.NodeRecord) (content.StableFile, error) {
	if err := source.authorize(record); err != nil {
		return nil, classifySourceError("open stable file", record.SourceReference(), err)
	}
	file, err := source.revisions.OpenStable(ctx, record)
	return file, classifySourceError("open stable file", record.SourceReference(), err)
}

func (source *SelectedFileSource) Close() error {
	if source == nil {
		return nil
	}
	source.closeOnce.Do(func() {
		if source.revisions != nil {
			source.closeErr = source.revisions.Close()
		}
		source.closeErr = errors.Join(source.closeErr, source.selected.Close())
	})
	return source.closeErr
}

func classifySourceError(operation string, reference catalog.SourceReference, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	failure := catalog.SourceFailureUnavailable
	switch {
	case errors.Is(cause, os.ErrPermission):
		failure = catalog.SourceFailureAccessDenied
	case errors.Is(cause, os.ErrNotExist), errors.Is(cause, content.ErrRevisionNotFound):
		failure = catalog.SourceFailureMissing
	case errors.Is(cause, content.ErrUnsupportedStability):
		failure = catalog.SourceFailureUnsupported
	case errors.Is(cause, content.ErrRevisionStale), errors.Is(cause, catalog.ErrDirectoryStale):
		failure = catalog.SourceFailureStale
	}
	return &catalog.SourceError{Operation: operation, Reference: reference, Failure: failure, Cause: cause}
}
