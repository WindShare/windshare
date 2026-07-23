package osfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

// StableBinding gives a platform/backend stability implementation an already
// root-confined handle plus the authenticated private catalog baseline. The
// binder takes ownership of File only when it returns a non-nil StableFile.
type StableBinding struct {
	File         *os.File
	Record       catalog.NodeRecord
	RootSlot     catalog.RootSlot
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
	mu     sync.RWMutex
	roots  []*os.Root
	binder StabilityBinder
	closer io.Closer
	closed bool
}

func NewRootedRevisionSource(config RootedRevisionSourceConfig) (*RootedRevisionSource, error) {
	return newRootedRevisionSource(config.RootPaths, config.Binder, nil)
}

func newOwnedRootedRevisionSource(rootPaths []string, binder ownedStabilityBinder) (*RootedRevisionSource, error) {
	return newRootedRevisionSource(rootPaths, binder, binder)
}

func newRootedRevisionSource(rootPaths []string, binder StabilityBinder, closer io.Closer) (*RootedRevisionSource, error) {
	if len(rootPaths) == 0 || len(rootPaths) > catalog.MaxRootSlots {
		return nil, fmt.Errorf("stable revision source requires 1..%d roots", catalog.MaxRootSlots)
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
			return nil, errors.Join(filesystemPathFailure("open stable source root", path, err), closeRoots(roots), closeOwnedBinder(closer))
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			return nil, errors.Join(filesystemPathFailure("open stable source root", absolute, err), closeRoots(roots), closeOwnedBinder(closer))
		}
		roots = append(roots, root)
	}
	if validator, ok := binder.(stabilityRootValidator); ok {
		if err := validator.ValidateRoots(roots); err != nil {
			return nil, errors.Join(err, closeRoots(roots), closeOwnedBinder(closer))
		}
	}
	return &RootedRevisionSource{roots: roots, binder: binder, closer: closer}, nil
}

func closeOwnedBinder(closer io.Closer) error {
	if closer == nil {
		return nil
	}
	return closer.Close()
}

func closeRoots(roots []*os.Root) error {
	var result error
	for index := len(roots) - 1; index >= 0; index-- {
		result = errors.Join(result, roots[index].Close())
	}
	return result
}

func (s *RootedRevisionSource) OpenStable(ctx context.Context, record catalog.NodeRecord) (content.StableFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fileID, isFile := record.FileID()
	if !isFile || fileID.IsZero() {
		return nil, content.ErrRevisionNotFound
	}
	locator := record.Locator()
	if locator.IsZero() || locator.RelativePath() == "" {
		return nil, content.ErrRevisionStale
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, content.ErrRevisionStoreClosed
	}
	slot := int(locator.RootSlot())
	if slot >= len(s.roots) {
		s.mu.RUnlock()
		return nil, content.ErrRevisionStale
	}
	root := s.roots[slot]
	before, err := root.Lstat(locator.RelativePath())
	if err != nil {
		s.mu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, content.ErrRevisionStale
		}
		return nil, filesystemPathFailure("inspect stable revision", locator.RelativePath(), err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || isReparsePoint(before) {
		s.mu.RUnlock()
		return nil, content.ErrRevisionStale
	}
	handle, err := root.Open(locator.RelativePath())
	if err != nil {
		s.mu.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, content.ErrRevisionStale
		}
		return nil, filesystemPathFailure("open stable revision", locator.RelativePath(), err)
	}
	after, lstatErr := root.Lstat(locator.RelativePath())
	s.mu.RUnlock()
	if lstatErr != nil {
		_ = handle.Close()
		if errors.Is(lstatErr, os.ErrNotExist) {
			return nil, content.ErrRevisionStale
		}
		return nil, filesystemPathFailure("reinspect stable revision", locator.RelativePath(), lstatErr)
	}
	owned := false
	defer func() {
		if !owned {
			_ = handle.Close()
		}
	}()
	info, err := handle.Stat()
	if err != nil {
		return nil, filesystemPathFailure("stat stable revision", locator.RelativePath(), err)
	}
	// Lstat on both sides of Open prevents a path replacement from being
	// accepted merely because its final target is regular. The binder then
	// matches the opened handle to the parent generation's private identity.
	if !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || isReparsePoint(after) ||
		!info.Mode().IsRegular() || isReparsePoint(info) || !os.SameFile(before, info) || !os.SameFile(after, info) ||
		uint64(info.Size()) != record.Entry().ExpectedSize() {
		return nil, content.ErrRevisionStale
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
	closer := s.closer
	s.closer = nil
	s.mu.Unlock()
	var result error
	for index := len(roots) - 1; index >= 0; index-- {
		result = errors.Join(result, roots[index].Close())
	}
	result = errors.Join(result, closeOwnedBinder(closer))
	return result
}
