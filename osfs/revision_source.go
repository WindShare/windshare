package osfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
)

// Bounded exponential backoff (~315ms total) aligns with output's
// windowsRenameBackoff to absorb brief 10-200ms antivirus, OneDrive,
// search-indexer, and editor auto-save locks without masking long-lived
// application locks such as an open Word/Excel document.
var revisionOpenSharingBackoff = [...]time.Duration{
	5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond,
	40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond,
}

type retryWaitFunc func(context.Context, time.Duration) error

func waitContextDelay(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func traceRevisionOpenRetry(
	sink diagnostics.Sink,
	stage string,
	slot RootSlot,
	attempts int,
	elapsed time.Duration,
	outcome string,
) {
	if !sink.Enabled() {
		return
	}
	sink.Emit(diagnostics.New("revision_open_sharing_retry", diagnostics.Fields{
		"stage":      diagnostics.Text(stage),
		"root_slot":  diagnostics.Uint(uint64(slot)),
		"attempts":   diagnostics.Uint(uint64(attempts)),
		"elapsed_ms": diagnostics.Uint(uint64(max(elapsed.Milliseconds(), 0))),
		"outcome":    diagnostics.Text(outcome),
	}))
}

// StableBinding gives a platform/backend stability implementation an already
// root-confined handle plus the authenticated private catalog baseline. The
// binder takes ownership of File only when it returns a non-nil StableFile.
type StableBinding struct {
	File         *os.File
	Record       catalog.NodeRecord
	RootSlot     RootSlot
	RelativePath string
}

type StabilityBinder interface {
	BindStable(context.Context, StableBinding) (content.StableFile, error)
}

type StabilityBinderFunc func(context.Context, StableBinding) (content.StableFile, error)

func (f StabilityBinderFunc) BindStable(ctx context.Context, binding StableBinding) (content.StableFile, error) {
	return f(ctx, binding)
}

type RootedRevisionSourceConfig struct {
	RootPaths []string
	Binder    StabilityBinder
}

type ownedStabilityBinder interface {
	StabilityBinder
	io.Closer
}

type stabilityRootValidator interface {
	ValidateRoots([]*os.Root) error
}

type RootedRevisionSource struct {
	mu          sync.RWMutex
	roots       []*os.Root
	inspector   rootInspector
	binder      StabilityBinder
	closer      io.Closer
	wait        retryWaitFunc
	diagnostics diagnostics.Sink
	closed      bool
}

func NewRootedRevisionSource(config RootedRevisionSourceConfig) (*RootedRevisionSource, error) {
	return newRootedRevisionSource(config.RootPaths, config.Binder, nil)
}

func newOwnedRootedRevisionSource(rootPaths []string, binder ownedStabilityBinder) (*RootedRevisionSource, error) {
	return newRootedRevisionSource(rootPaths, binder, binder)
}

func newRootedRevisionSource(rootPaths []string, binder StabilityBinder, closer io.Closer) (*RootedRevisionSource, error) {
	if len(rootPaths) == 0 || len(rootPaths) > catalog.MaxSelectedRoots {
		return nil, fmt.Errorf("stable revision source requires 1..%d roots", catalog.MaxSelectedRoots)
	}
	if binder == nil {
		// os.Root provides containment but does not by itself prove the v2
		// no-writer/monotonic-token stability requirement.
		return nil, content.ErrUnsupportedStability
	}
	roots := make([]*os.Root, 0, len(rootPaths))
	for _, path := range rootPaths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, errors.Join(pathfailure.Filesystem("open stable source root", path, err), closeRoots(roots), closeOwnedBinder(closer))
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			return nil, errors.Join(pathfailure.Filesystem("open stable source root", absolute, err), closeRoots(roots), closeOwnedBinder(closer))
		}
		roots = append(roots, root)
	}
	return adoptRootedRevisionSource(roots, binder, closer, diagnostics.Sink{})
}

func newRetainedRootedRevisionSource(
	retained []*os.Root,
	binder StabilityBinder,
	closer io.Closer,
	sink diagnostics.Sink,
) (*RootedRevisionSource, error) {
	if len(retained) == 0 || len(retained) > catalog.MaxSelectedRoots || binder == nil {
		return nil, errors.Join(content.ErrUnsupportedStability, closeOwnedBinder(closer))
	}
	roots := make([]*os.Root, 0, len(retained))
	for _, authority := range retained {
		root, err := authority.OpenRoot(".")
		if err != nil {
			return nil, errors.Join(err, closeRoots(roots), closeOwnedBinder(closer))
		}
		roots = append(roots, root)
	}
	return adoptRootedRevisionSource(roots, binder, closer, sink)
}

// Both construction paths transfer independent root ownership here. The binder
// must prove that any additional native handles describe these exact roots.
func adoptRootedRevisionSource(
	roots []*os.Root,
	binder StabilityBinder,
	closer io.Closer,
	sink diagnostics.Sink,
) (*RootedRevisionSource, error) {
	if validator, ok := binder.(stabilityRootValidator); ok {
		if err := validator.ValidateRoots(roots); err != nil {
			return nil, errors.Join(err, closeRoots(roots), closeOwnedBinder(closer))
		}
	}
	inspector, err := newRootInspector(roots)
	if err != nil {
		return nil, errors.Join(err, closeRoots(roots), closeOwnedBinder(closer))
	}
	return &RootedRevisionSource{roots: roots, inspector: inspector, binder: binder, closer: closer, diagnostics: sink}, nil
}

func closeOwnedBinder(closer io.Closer) error {
	if closer == nil {
		return nil
	}
	return closer.Close()
}

func closeRoots(roots []*os.Root) error {
	var result error
	for index := range slices.Backward(roots) {
		result = errors.Join(result, roots[index].Close())
	}
	return result
}

func (s *RootedRevisionSource) waitForRetry(ctx context.Context, delay time.Duration) error {
	if s.wait != nil {
		return s.wait(ctx, delay)
	}
	return waitContextDelay(ctx, delay)
}

func (s *RootedRevisionSource) openPreliminaryOnce(
	slot int,
	relative string,
) (*os.File, sourceObjectIdentity, sourceObjectIdentity, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, content.WithRevisionComparison(
			content.ErrRevisionStoreClosed, content.RevisionComparisonUnavailable,
		)
	}
	if slot < 0 || slot >= len(s.roots) {
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, content.WithRevisionComparison(
			content.ErrRevisionStale, content.RevisionComparisonMismatch,
		)
	}
	root := s.roots[slot]
	before, beforeOK, err := s.inspector.inspectRelativeRegularFile(root, slot, relative)
	if err != nil {
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, classifyRootRelativeOpenError("inspect stable revision", relative, err)
	}
	if !beforeOK {
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, content.WithRevisionComparison(
			content.ErrRevisionStale, content.RevisionComparisonMismatch,
		)
	}
	handle, err := openRootObjectNoRecall(root, relative)
	if err != nil {
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, classifyRootRelativeOpenError("open stable revision", relative, err)
	}
	after, afterOK, afterErr := s.inspector.inspectRelativeRegularFile(root, slot, relative)
	if afterErr != nil {
		_ = handle.Close()
		return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, classifyRootRelativeOpenError("reinspect stable revision", relative, afterErr)
	}
	return handle, before, after, afterOK, nil
}

func (s *RootedRevisionSource) openPreliminaryWithBackoff(
	ctx context.Context,
	slot RootSlot,
	relative string,
) (*os.File, sourceObjectIdentity, sourceObjectIdentity, bool, error) {
	handle, before, after, afterOK, err := s.openPreliminaryOnce(int(slot), relative)
	if err == nil || !isTransientRevisionOpenError(err) {
		return handle, before, after, afterOK, err
	}
	started := time.Now()
	attempts := 1
	// Wait outside s.mu.RLock so a single locked file never blocks Close() or
	// sibling opens while absorbing brief external sharing violations.
	for _, delay := range revisionOpenSharingBackoff {
		if waitErr := s.waitForRetry(ctx, delay); waitErr != nil {
			traceRevisionOpenRetry(s.diagnostics, "preliminary", slot, attempts, time.Since(started), "canceled")
			return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, content.WithRevisionComparison(
				waitErr, content.RevisionComparisonUnavailable,
			)
		}
		attempts++
		handle, before, after, afterOK, err = s.openPreliminaryOnce(int(slot), relative)
		if err == nil {
			traceRevisionOpenRetry(s.diagnostics, "preliminary", slot, attempts, time.Since(started), "recovered")
			return handle, before, after, afterOK, nil
		}
		if !isTransientRevisionOpenError(err) {
			traceRevisionOpenRetry(s.diagnostics, "preliminary", slot, attempts, time.Since(started), "failed")
			return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, err
		}
	}
	traceRevisionOpenRetry(s.diagnostics, "preliminary", slot, attempts, time.Since(started), "exhausted")
	return nil, sourceObjectIdentity{}, sourceObjectIdentity{}, false, err
}

func (s *RootedRevisionSource) OpenStable(ctx context.Context, record catalog.NodeRecord) (content.StableFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, content.WithRevisionComparison(err, content.RevisionComparisonUnavailable)
	}
	fileID, isFile := record.FileID()
	if !isFile || fileID.IsZero() {
		return nil, content.WithRevisionComparison(content.ErrRevisionNotFound, content.RevisionComparisonUnavailable)
	}
	locator, err := parseSourceReference(record.SourceReference())
	if err != nil || locator.RelativePath() == "" {
		return nil, content.WithRevisionComparison(content.ErrRevisionStale, content.RevisionComparisonMismatch)
	}
	handle, before, after, afterOK, err := s.openPreliminaryWithBackoff(ctx, locator.RootSlot(), locator.RelativePath())
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			_ = handle.Close()
		}
	}()
	opened, err := inspectOpenedSourceHandle(handle)
	if err != nil {
		return nil, content.WithRevisionComparison(
			pathfailure.Filesystem("classify stable revision", locator.RelativePath(), err), content.RevisionComparisonUnavailable,
		)
	}
	// No-follow metadata inspection on both sides of Open prevents a path
	// replacement from being accepted merely because its final target is regular.
	// The binder then matches the opened handle to the parent generation's private identity.
	if !afterOK || opened.kind != sourceEntryRegularFile ||
		!before.sameObject(opened.identity) || !after.sameObject(opened.identity) ||
		opened.size != record.Entry().ExpectedSize() {
		return nil, content.WithRevisionComparison(content.ErrRevisionStale, content.RevisionComparisonMismatch)
	}
	stable, err := s.binder.BindStable(ctx, StableBinding{
		File: handle, Record: record, RootSlot: locator.RootSlot(), RelativePath: locator.RelativePath(),
	})
	if err != nil {
		return nil, err
	}
	if stable == nil {
		return nil, errors.New("stability binder returned a nil stable file")
	}
	owned = true
	return stable, nil
}

func (s *RootedRevisionSource) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	roots := s.roots
	s.roots = nil
	inspector := s.inspector
	s.inspector = rootInspector{}
	closer := s.closer
	s.closer = nil
	s.mu.Unlock()
	result := inspector.Close()
	for index := range slices.Backward(roots) {
		result = errors.Join(result, roots[index].Close())
	}
	result = errors.Join(result, closeOwnedBinder(closer))
	return result
}
