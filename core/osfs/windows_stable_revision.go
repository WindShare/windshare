//go:build windows

package osfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/pathfailure"
	"golang.org/x/sys/windows"
)

func platformCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return windowsCatalogObjectBaseline(file)
}

func newPlatformRootedRevisionSource(paths []string) (*RootedRevisionSource, error) {
	return NewWindowsRootedRevisionSource(paths)
}

type WindowsStabilityBinder struct {
	mu       sync.RWMutex
	platform windowsRevisionPlatform
	roots    []windowsRevisionRoot
	closed   bool
}

func NewWindowsStabilityBinder(rootPaths []string) (*WindowsStabilityBinder, error) {
	return newWindowsStabilityBinder(rootPaths, nativeWindowsRevisionPlatform{})
}

func newWindowsStabilityBinder(rootPaths []string, platform windowsRevisionPlatform) (*WindowsStabilityBinder, error) {
	if len(rootPaths) == 0 || len(rootPaths) > catalog.MaxRootSlots || platform == nil {
		return nil, content.ErrUnsupportedStability
	}
	roots := make([]windowsRevisionRoot, 0, len(rootPaths))
	for _, path := range rootPaths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, errors.Join(pathfailure.Filesystem("resolve Windows stable root", path, err), closeWindowsRevisionRoots(roots))
		}
		root, err := platform.OpenRoot(absolute)
		if err != nil {
			return nil, errors.Join(pathfailure.Filesystem("open Windows stable root", absolute, err), closeWindowsRevisionRoots(roots))
		}
		roots = append(roots, root)
	}
	return &WindowsStabilityBinder{platform: platform, roots: roots}, nil
}

// NewWindowsRootedRevisionSource is the R6 Windows factory. The native binder
// and its retained root handles are owned by the returned source and close with
// it; callers cannot accidentally substitute path-based reopen semantics.
func NewWindowsRootedRevisionSource(rootPaths []string) (*RootedRevisionSource, error) {
	binder, err := NewWindowsStabilityBinder(rootPaths)
	if err != nil {
		return nil, err
	}
	return newOwnedRootedRevisionSource(rootPaths, binder)
}

// WindowsCatalogBaseline captures the private catalog candidate from the
// already-open object. The later root-relative stable open must reproduce these
// exact values before a revision descriptor can be published.
func WindowsCatalogBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	if file == nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
	}
	if err := ensureSupportedWindowsRevisionVolume(windows.Handle(file.Fd())); err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	token, err := inspectWindowsMutationToken(windows.Handle(file.Fd()))
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, classifyWindowsIdentityError(err)
	}
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	return identity, candidate, nil
}

// windowsCatalogObjectBaseline extends the private catalog identity boundary to
// directories. Revision publication still uses WindowsCatalogBaseline and
// rejects directories; lazy catalog scans need the directory ChangeTime token
// so a generation cannot commit across an enumeration mutation.
func windowsCatalogObjectBaseline(file *os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	if file == nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
	}
	handle := windows.Handle(file.Fd())
	if err := ensureSupportedWindowsRevisionVolume(handle); err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	token, err := inspectWindowsCatalogToken(handle)
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, classifyWindowsIdentityError(err)
	}
	identity, err := catalog.NewSourceIdentity(token.sourceIdentityBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	candidate, err := catalog.NewVersionCandidate(token.candidateBytes())
	if err != nil {
		return catalog.SourceIdentity{}, catalog.VersionCandidate{}, err
	}
	return identity, candidate, nil
}

func (b *WindowsStabilityBinder) BindStable(ctx context.Context, binding StableBinding) (content.StableFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.File == nil || binding.RelativePath == "" {
		return nil, content.ErrUnsupportedStability
	}
	relative := filepath.FromSlash(binding.RelativePath)
	if !filepath.IsLocal(relative) {
		return nil, content.ErrRevisionStale
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, content.ErrRevisionStoreClosed
	}
	slot := int(binding.RootSlot)
	if slot < 0 || slot >= len(b.roots) {
		return nil, content.ErrRevisionStale
	}
	before, err := b.platform.Token(binding.File)
	if err != nil {
		return nil, fmt.Errorf("inspect Windows revision before stable open: %w", err)
	}
	if !before.matches(binding.Record) {
		return nil, content.ErrRevisionStale
	}
	handle, err := b.roots[slot].OpenStable(relative)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			_ = handle.Close()
		}
	}()
	after, err := b.platform.Token(binding.File)
	if err != nil {
		return nil, fmt.Errorf("inspect Windows revision after stable open: %w", err)
	}
	stableToken, err := handle.Token()
	if err != nil {
		return nil, fmt.Errorf("inspect write-excluding Windows revision: %w", err)
	}
	if before != after || after != stableToken || !stableToken.matches(binding.Record) {
		return nil, content.ErrRevisionStale
	}
	modified, err := stableToken.modifiedTime()
	if err != nil {
		return nil, fmt.Errorf("represent Windows revision modified time: %w", err)
	}
	if err := binding.File.Close(); err != nil {
		return nil, fmt.Errorf("close preliminary Windows revision handle: %w", err)
	}
	owned = true
	return &windowsStableFile{handle: handle, baseline: stableToken, modified: modified}, nil
}

func (b *WindowsStabilityBinder) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	roots := b.roots
	b.roots = nil
	b.mu.Unlock()
	return closeWindowsRevisionRoots(roots)
}

func (b *WindowsStabilityBinder) ValidateRoots(roots []*os.Root) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return content.ErrRevisionStoreClosed
	}
	if len(roots) != len(b.roots) {
		return content.ErrUnsupportedStability
	}
	for index, root := range roots {
		file, err := root.Open(".")
		if err != nil {
			return fmt.Errorf("open Windows root authority %d: %w", index, err)
		}
		osIdentity, identityErr := inspectWindowsPersistentFileIdentity(windows.Handle(file.Fd()))
		closeErr := file.Close()
		if identityErr != nil || closeErr != nil {
			return fmt.Errorf("inspect Windows root authority %d: %w", index, errors.Join(identityErr, closeErr))
		}
		nativeIdentity, err := b.roots[index].Identity()
		if err != nil {
			return fmt.Errorf("inspect native Windows root authority %d: %w", index, err)
		}
		if nativeIdentity != osIdentity {
			return fmt.Errorf("%w: Windows root authority changed while it was retained", content.ErrUnsupportedStability)
		}
	}
	return nil
}

func closeWindowsRevisionRoots(roots []windowsRevisionRoot) error {
	var result error
	for index := len(roots) - 1; index >= 0; index-- {
		result = errors.Join(result, roots[index].Close())
	}
	return result
}

type windowsStableFile struct {
	mu       sync.RWMutex
	handle   windowsRevisionFile
	baseline windowsMutationToken
	modified catalog.ModifiedTime
	closed   bool
}

func (f *windowsStableFile) ExactSize() uint64                  { return f.baseline.size }
func (f *windowsStableFile) ModifiedTime() catalog.ModifiedTime { return f.modified }

func (f *windowsStableFile) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return content.ErrSourceDrift
	}
	return f.verifyLocked()
}

func (f *windowsStableFile) verifyLocked() error {
	current, err := f.handle.Token()
	if err != nil {
		return fmt.Errorf("inspect Windows stable source: %w", err)
	}
	if !current.sameOpenedRevision(f.baseline) {
		return content.ErrSourceDrift
	}
	return nil
}

func (f *windowsStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset > math.MaxInt64 || uint64(len(destination)) > math.MaxInt64-offset {
		return 0, content.ErrBlockOutOfRange
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return 0, content.ErrSourceDrift
	}
	if err := f.verifyLocked(); err != nil {
		return 0, err
	}
	count, readErr := f.handle.ReadAt(destination, int64(offset))
	if err := f.verifyLocked(); err != nil {
		return count, err
	}
	if errors.Is(readErr, io.EOF) && count == len(destination) {
		return count, nil
	}
	return count, readErr
}

func (f *windowsStableFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.handle.Close()
}
