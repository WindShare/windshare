package osfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const catalogEnumerationBatchSize = 128

type CatalogIdentitySource interface {
	NewCatalogIdentity() ([catalog.IdentityBytes]byte, error)
}

type CatalogIdentitySourceFunc func() ([catalog.IdentityBytes]byte, error)

func (function CatalogIdentitySourceFunc) NewCatalogIdentity() ([catalog.IdentityBytes]byte, error) {
	if function == nil {
		return [catalog.IdentityBytes]byte{}, errors.New("osfs: catalog identity source is nil")
	}
	return function()
}

type SelectedCatalogSourceConfig struct {
	Paths         []string
	SyntheticRoot catalog.DirectoryID
	Identities    CatalogIdentitySource
}

// SelectedCatalogSource opens only the explicitly selected roots during share
// preparation. Descendants are enumerated through ScanDirectory after relay
// registration, preserving the user-visible ready-before-traversal contract.
type SelectedCatalogSource struct {
	mu         sync.RWMutex
	roots      []*os.Root
	rootPaths  []string
	selected   []catalog.NodeRecord
	identities CatalogIdentitySource
	used       map[catalog.NodeID]struct{}
	closed     bool
}

func NewSelectedCatalogSource(config SelectedCatalogSourceConfig) (*SelectedCatalogSource, error) {
	if len(config.Paths) == 0 || len(config.Paths) > catalog.MaxRootSlots || config.SyntheticRoot.IsZero() {
		return nil, fmt.Errorf("osfs: selected catalog source requires 1..%d roots and a synthetic root", catalog.MaxRootSlots)
	}
	if config.Identities == nil {
		config.Identities = CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
			var identity [catalog.IdentityBytes]byte
			_, err := io.ReadFull(rand.Reader, identity[:])
			return identity, err
		})
	}
	source := &SelectedCatalogSource{
		identities: config.Identities, used: map[catalog.NodeID]struct{}{config.SyntheticRoot.NodeID(): {}},
	}
	fail := func(cause error) (*SelectedCatalogSource, error) {
		return nil, errors.Join(cause, source.Close())
	}
	for index, selectedPath := range config.Paths {
		record, root, rootPath, err := source.openSelectedRoot(selectedPath, catalog.RootSlot(index), config.SyntheticRoot)
		if err != nil {
			return fail(err)
		}
		source.roots = append(source.roots, root)
		source.rootPaths = append(source.rootPaths, rootPath)
		source.selected = append(source.selected, record)
	}
	return source, nil
}

func (source *SelectedCatalogSource) openSelectedRoot(
	selectedPath string,
	slot catalog.RootSlot,
	parent catalog.DirectoryID,
) (catalog.NodeRecord, *os.Root, string, error) {
	absolute, err := filepath.Abs(selectedPath)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", filesystemPathFailure("resolve selected root", selectedPath, err)
	}
	before, err := os.Lstat(absolute)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", filesystemPathFailure("inspect selected root", absolute, err)
	}
	if isReparsePoint(before) || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() && !before.Mode().IsRegular() {
		return catalog.NodeRecord{}, nil, "", fmt.Errorf("osfs: selected root %q is not a stable file or directory", absolute)
	}
	rootPath, relative := filepath.Dir(absolute), filepath.Base(absolute)
	if before.IsDir() {
		rootPath, relative = absolute, ""
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", filesystemPathFailure("open selected root authority", rootPath, err)
	}
	openName := relative
	if openName == "" {
		openName = "."
	}
	handle, err := root.Open(openName)
	if err != nil {
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", filesystemPathFailure("open selected root object", absolute, err)
	}
	opened, statErr := handle.Stat()
	identity, candidate, baselineErr := platformCatalogBaseline(handle)
	closeErr := handle.Close()
	if err := errors.Join(statErr, baselineErr, closeErr); err != nil {
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", errors.Join(fmt.Errorf("inspect selected root object: %w", err), root.Close())
	}
	if opened.IsDir() != before.IsDir() || !os.SameFile(before, opened) {
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", errors.Join(catalog.ErrDirectoryStale, root.Close())
	}
	modified, err := catalogModifiedTime(opened)
	if err != nil {
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", errors.Join(err, root.Close())
	}
	locator, err := catalog.NewLocator(slot, filepath.ToSlash(relative))
	if err != nil {
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", errors.Join(err, root.Close())
	}
	name := filepath.Base(absolute)
	if opened.IsDir() {
		directory, idErr := source.newDirectoryID()
		if idErr == nil {
			var record catalog.NodeRecord
			record, idErr = catalog.NewDirectoryNodeRecord(directory, parent, name, locator, identity, modified)
			if idErr == nil {
				return record, root, rootPath, nil
			}
		}
		_ = root.Close()
		return catalog.NodeRecord{}, nil, "", idErr
	}
	file, err := source.newFileID()
	if err == nil {
		var record catalog.NodeRecord
		record, err = catalog.NewFileNodeRecord(file, parent, name, locator, identity, candidate, uint64(opened.Size()), modified)
		if err == nil {
			return record, root, rootPath, nil
		}
	}
	_ = root.Close()
	return catalog.NodeRecord{}, nil, "", err
}

func (source *SelectedCatalogSource) SelectedRoots() []catalog.NodeRecord {
	source.mu.RLock()
	defer source.mu.RUnlock()
	return append([]catalog.NodeRecord(nil), source.selected...)
}

func (source *SelectedCatalogSource) RevisionSource() (*RootedRevisionSource, error) {
	source.mu.RLock()
	if source.closed {
		source.mu.RUnlock()
		return nil, content.ErrRevisionStoreClosed
	}
	paths := append([]string(nil), source.rootPaths...)
	source.mu.RUnlock()
	return newPlatformRootedRevisionSource(paths)
}

func (source *SelectedCatalogSource) ScanDirectory(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return catalog.ScanResult{}, err
	}
	directoryID, directoryKind := request.Directory.DirectoryID()
	if !directoryKind || directoryID.IsZero() || request.Work == nil || request.Children == nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.New("osfs: invalid catalog scan request"))
	}
	locator := request.Directory.Locator()
	source.mu.RLock()
	if source.closed || int(locator.RootSlot()) >= len(source.roots) {
		source.mu.RUnlock()
		return catalog.ScanResult{}, catalog.NewPermanentScanError(content.ErrRevisionStoreClosed)
	}
	root := source.roots[locator.RootSlot()]
	source.mu.RUnlock()
	path := filepath.FromSlash(locator.RelativePath())
	if path == "" {
		path = "."
	}
	directory, err := root.OpenRoot(path)
	if err != nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	defer directory.Close()
	handle, err := directory.Open(".")
	if err != nil {
		return catalog.ScanResult{}, catalog.NewTransientScanError(err, 0)
	}
	defer handle.Close()
	beforeIdentity, beforeCandidate, err := platformCatalogBaseline(handle)
	if err != nil || !bytes.Equal(beforeIdentity.Bytes(), request.Directory.SourceIdentity().Bytes()) {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	result, beforeEntries, err := source.enumerateCatalogChildren(ctx, directory, handle, locator, request)
	if err != nil {
		return catalog.ScanResult{}, err
	}
	afterHandle, err := directory.Open(".")
	if err != nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	afterIdentity, afterCandidate, baselineErr := platformCatalogBaseline(afterHandle)
	afterEntries, snapshotErr := catalogDirectoryFingerprint(ctx, afterHandle)
	closeErr := afterHandle.Close()
	if err := errors.Join(baselineErr, snapshotErr, closeErr); err != nil ||
		!bytes.Equal(beforeIdentity.Bytes(), afterIdentity.Bytes()) ||
		!bytes.Equal(beforeCandidate.Bytes(), afterCandidate.Bytes()) ||
		!bytes.Equal(beforeEntries[:], afterEntries[:]) {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	return result, nil
}

func (source *SelectedCatalogSource) enumerateCatalogChildren(
	ctx context.Context,
	directory *os.Root,
	handle *os.File,
	locator catalog.Locator,
	request catalog.ScanRequest,
) (catalog.ScanResult, [sha256.Size]byte, error) {
	result := catalog.ScanResult{}
	fingerprint := newCatalogDirectoryFingerprint()
	for {
		entries, readErr := handle.ReadDir(catalogEnumerationBatchSize)
		for _, entry := range entries {
			fingerprint.add(entry)
			if err := request.Work.Consume(1); err != nil {
				return catalog.ScanResult{}, [sha256.Size]byte{}, err
			}
			child, omitted, err := source.scanChild(ctx, directory, locator, entry)
			if err != nil {
				return catalog.ScanResult{}, [sha256.Size]byte{}, err
			}
			if omitted {
				result.OmittedCount++
				continue
			}
			if err := request.Children.Add(ctx, child); err != nil {
				return catalog.ScanResult{}, [sha256.Size]byte{}, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return result, fingerprint.sum(), nil
		}
		if readErr != nil {
			return catalog.ScanResult{}, [sha256.Size]byte{}, catalog.NewTransientScanError(readErr, 0)
		}
	}
}

type catalogDirectoryFingerprintState struct {
	digest hash.Hash
	count  uint64
}

func newCatalogDirectoryFingerprint() *catalogDirectoryFingerprintState {
	return &catalogDirectoryFingerprintState{digest: sha256.New()}
}

func (fingerprint *catalogDirectoryFingerprintState) add(entry fs.DirEntry) {
	name := []byte(entry.Name())
	var header [12]byte
	binary.BigEndian.PutUint64(header[:8], uint64(len(name)))
	binary.BigEndian.PutUint32(header[8:], uint32(entry.Type()&fs.ModeType))
	_, _ = fingerprint.digest.Write(header[:])
	_, _ = fingerprint.digest.Write(name)
	fingerprint.count++
}

func (fingerprint *catalogDirectoryFingerprintState) sum() [sha256.Size]byte {
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], fingerprint.count)
	_, _ = fingerprint.digest.Write(count[:])
	var result [sha256.Size]byte
	copy(result[:], fingerprint.digest.Sum(nil))
	return result
}

func catalogDirectoryFingerprint(ctx context.Context, handle *os.File) ([sha256.Size]byte, error) {
	fingerprint := newCatalogDirectoryFingerprint()
	for {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		entries, err := handle.ReadDir(catalogEnumerationBatchSize)
		for _, entry := range entries {
			fingerprint.add(entry)
		}
		if errors.Is(err, io.EOF) {
			return fingerprint.sum(), nil
		}
		if err != nil {
			return [sha256.Size]byte{}, err
		}
	}
}

func (source *SelectedCatalogSource) scanChild(
	ctx context.Context,
	directory *os.Root,
	parentLocator catalog.Locator,
	entry fs.DirEntry,
) (catalog.ScannedChild, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.ScannedChild{}, false, err
	}
	name := entry.Name()
	before, err := directory.Lstat(name)
	if err != nil {
		return catalog.ScannedChild{}, false, catalog.NewTransientScanError(err, 0)
	}
	if isReparsePoint(before) || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() && !before.Mode().IsRegular() {
		return catalog.ScannedChild{}, true, nil
	}
	handle, err := directory.Open(name)
	if err != nil {
		return catalog.ScannedChild{}, false, catalog.NewTransientScanError(err, 0)
	}
	opened, statErr := handle.Stat()
	identity, candidate, baselineErr := platformCatalogBaseline(handle)
	closeErr := handle.Close()
	if err := errors.Join(statErr, baselineErr, closeErr); err != nil {
		return catalog.ScannedChild{}, false, catalog.NewTransientScanError(err, 0)
	}
	if opened.IsDir() != before.IsDir() || !os.SameFile(before, opened) {
		return catalog.ScannedChild{}, false, catalog.NewPermanentScanError(catalog.ErrDirectoryStale)
	}
	modified, err := catalogModifiedTime(opened)
	if err != nil {
		return catalog.ScannedChild{}, false, catalog.NewPermanentScanError(err)
	}
	relative := name
	if parentLocator.RelativePath() != "" {
		relative = parentLocator.RelativePath() + "/" + name
	}
	locator, err := catalog.NewLocator(parentLocator.RootSlot(), filepath.ToSlash(relative))
	if err != nil {
		return catalog.ScannedChild{}, false, catalog.NewPermanentScanError(err)
	}
	child := catalog.ScannedChild{Name: name, Locator: locator, SourceIdentity: identity, ModifiedTime: modified}
	if opened.IsDir() {
		child.DirectoryID, err = source.newDirectoryID()
	} else {
		child.FileID, err = source.newFileID()
		child.VersionCandidate = candidate
		child.ExpectedSize = uint64(opened.Size())
	}
	if err != nil {
		return catalog.ScannedChild{}, false, err
	}
	return child, false, nil
}

func catalogModifiedTime(information fs.FileInfo) (catalog.ModifiedTime, error) {
	modified := information.ModTime()
	return catalog.NewModifiedTime(modified.Unix(), uint32(modified.Nanosecond()), catalog.TimePrecisionNanoseconds)
}

func (source *SelectedCatalogSource) newDirectoryID() (catalog.DirectoryID, error) {
	identity, err := source.newNodeIdentity()
	if err != nil {
		return catalog.DirectoryID{}, err
	}
	return catalog.DirectoryIDFromBytes(identity[:])
}

func (source *SelectedCatalogSource) newFileID() (catalog.FileID, error) {
	identity, err := source.newNodeIdentity()
	if err != nil {
		return catalog.FileID{}, err
	}
	return catalog.FileIDFromBytes(identity[:])
}

func (source *SelectedCatalogSource) newNodeIdentity() ([catalog.IdentityBytes]byte, error) {
	for range 4 {
		identity, err := source.identities.NewCatalogIdentity()
		if err != nil {
			return identity, err
		}
		node, err := catalog.NodeIDFromBytes(identity[:])
		if err != nil || node.IsZero() {
			continue
		}
		source.mu.Lock()
		_, collision := source.used[node]
		if !collision {
			source.used[node] = struct{}{}
		}
		source.mu.Unlock()
		if !collision {
			return identity, nil
		}
	}
	return [catalog.IdentityBytes]byte{}, errors.New("osfs: catalog identity source repeatedly collided")
}

func (source *SelectedCatalogSource) Close() error {
	if source == nil {
		return nil
	}
	source.mu.Lock()
	if source.closed {
		source.mu.Unlock()
		return nil
	}
	source.closed = true
	roots := source.roots
	source.roots = nil
	source.mu.Unlock()
	return closeRoots(roots)
}
