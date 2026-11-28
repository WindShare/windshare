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
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
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
	Diagnostics   diagnostics.Sink
	Omissions     catalog.SourceOmissionObserver
}

// SelectedCatalogSource opens only the explicitly selected roots during share
// preparation. Descendants are enumerated through ScanDirectory after relay
// registration, preserving the user-visible ready-before-traversal contract.
type SelectedCatalogSource struct {
	mu                   sync.RWMutex
	roots                []*os.Root
	rootDisplayPaths     []string
	selected             []catalog.NodeRecord
	identities           CatalogIdentitySource
	diagnostics          diagnostics.Sink
	omissions            catalog.SourceOmissionObserver
	used                 map[catalog.NodeID]struct{}
	closed               bool
	inspectChildHandleFn func(string, *os.File) (openedSourceHandle, error)
}

func NewSelectedCatalogSource(config SelectedCatalogSourceConfig) (*SelectedCatalogSource, error) {
	return newSelectedCatalogSource(context.Background(), config)
}

func newSelectedCatalogSource(ctx context.Context, config SelectedCatalogSourceConfig) (*SelectedCatalogSource, error) {
	if len(config.Paths) == 0 || len(config.Paths) > catalog.MaxSelectedRoots || config.SyntheticRoot.IsZero() {
		return nil, fmt.Errorf("osfs: selected catalog source requires 1..%d roots and a synthetic root", catalog.MaxSelectedRoots)
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
		diagnostics: config.Diagnostics, omissions: config.Omissions,
	}
	fail := func(cause error) (*SelectedCatalogSource, error) {
		return nil, errors.Join(cause, source.Close())
	}
	selectedNames := make(map[string]int, len(config.Paths))
	for index, selectedPath := range config.Paths {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		record, root, displayPath, err := source.openSelectedRoot(selectedPath, RootSlot(index), config.SyntheticRoot)
		if err != nil {
			return fail(&catalog.SelectionError{
				Label: selectedPath, Cause: classifySourceError("acquire selected root", catalog.SourceReference{}, err),
			})
		}
		nameKey := catalog.SiblingNameKey(record.Entry().Name())
		if position, collides := selectedNames[nameKey]; collides {
			// Selected roots become siblings under one synthetic root. Preserve both
			// selections before the catalog loses their caller-facing labels.
			collision := &catalog.SelectionNameCollisionError{
				FirstLabel: config.Paths[position], SecondLabel: selectedPath,
			}
			return fail(errors.Join(collision, root.Close()))
		}
		selectedNames[nameKey] = index
		source.roots = append(source.roots, root)
		source.rootDisplayPaths = append(source.rootDisplayPaths, displayPath)
		source.selected = append(source.selected, record)
	}
	return source, nil
}

func (source *SelectedCatalogSource) openSelectedRoot(
	selectedPath string,
	slot RootSlot,
	parent catalog.DirectoryID,
) (catalog.NodeRecord, *os.Root, string, error) {
	if selectedPath == "" {
		// filepath.Abs("") is the working directory; an empty selection (typically an
		// unset variable) must not silently publish it. Report it as any other
		// missing root.
		return catalog.NodeRecord{}, nil, "", pathfailure.Filesystem("resolve selected root", selectedPath, fs.ErrNotExist)
	}
	absolute, err := filepath.Abs(selectedPath)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", pathfailure.Filesystem("resolve selected root", selectedPath, err)
	}
	before, kind, beforeAudit, err := inspectSelectedRootKind(absolute)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", pathfailure.Filesystem("inspect selected root", absolute, err)
	}
	openTarget := absolute
	resolvedLinkedCWD := false
	if isPureDotTraversal(selectedPath) && isLinkedDirectoryLeaf(kind, beforeAudit) {
		resolved, resolveErr := resolveLinkedWorkingDirectory(selectedPath, absolute)
		if resolveErr != nil {
			return catalog.NodeRecord{}, nil, "", resolveErr
		}
		before, kind, beforeAudit = resolved.identity, resolved.kind, resolved.audit
		if resolved.openPath != "" {
			openTarget = resolved.openPath
			resolvedLinkedCWD = true
		}
	}
	switch kind {
	case sourceEntryDirectory, sourceEntryRegularFile:
	case sourceEntryCloudPlaceholder:
		source.traceSelectedRootPolicy(slot, "cloud_placeholder", beforeAudit)
		return catalog.NodeRecord{}, nil, "", fmt.Errorf("osfs: selected root %q is not fully available on this device: %w", absolute, errSourceCloudPlaceholder)
	default:
		if kind == sourceEntryReparsePoint {
			source.traceSelectedRootPolicy(slot, "rejected_reparse", beforeAudit)
		}
		return catalog.NodeRecord{}, nil, "", fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	rootPath, relative := filepath.Dir(openTarget), filepath.Base(openTarget)
	if kind == sourceEntryDirectory {
		rootPath, relative = openTarget, ""
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		openErrPath := rootPath
		if resolvedLinkedCWD {
			openErrPath = absolute
		}
		return catalog.NodeRecord{}, nil, "", pathfailure.Filesystem("open selected root authority", openErrPath, err)
	}
	ownedRoot := false
	defer func() {
		if !ownedRoot {
			_ = root.Close()
		}
	}()
	opened, identity, candidate, err := source.openSelectedRootBaseline(root, absolute, relative, slot, before, kind)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", err
	}
	record, err := source.buildSelectedRootRecord(absolute, relative, slot, parent, kind, opened, identity, candidate)
	if err != nil {
		return catalog.NodeRecord{}, nil, "", err
	}
	if resolvedLinkedCWD {
		source.traceSelectedRootLinkedCWD(slot)
	}
	ownedRoot = true
	return record, root, absolute, nil
}

func (source *SelectedCatalogSource) openSelectedRootBaseline(
	root *os.Root,
	absolute string,
	relative string,
	slot RootSlot,
	before sourceObjectIdentity,
	kind sourceEntryKind,
) (openedSourceHandle, catalog.SourceIdentity, catalog.VersionCandidate, error) {
	openName := relative
	if openName == "" {
		openName = "."
	}
	handle, err := openRootObjectNoRecall(root, openName)
	if err != nil {
		return openedSourceHandle{}, catalog.SourceIdentity{}, catalog.VersionCandidate{}, pathfailure.Filesystem("open selected root object", absolute, err)
	}
	opened, inspectErr := inspectOpenedSourceHandle(handle)
	if inspectErr != nil {
		return openedSourceHandle{}, catalog.SourceIdentity{}, catalog.VersionCandidate{}, errors.Join(fmt.Errorf("inspect selected root object: %w", inspectErr), handle.Close())
	}
	sameObject := before.sameObject(opened.identity)
	if sameObject && opened.kind == sourceEntryCloudPlaceholder {
		source.traceSelectedRootPolicy(slot, "cloud_placeholder", opened.audit)
		return openedSourceHandle{}, catalog.SourceIdentity{}, catalog.VersionCandidate{}, errors.Join(
			fmt.Errorf("osfs: selected root %q is not fully available on this device: %w", absolute, errSourceCloudPlaceholder),
			handle.Close(),
		)
	}
	if !sameObject || opened.kind != kind {
		return openedSourceHandle{}, catalog.SourceIdentity{}, catalog.VersionCandidate{}, errors.Join(catalog.ErrDirectoryStale, handle.Close())
	}
	identity, candidate, baselineErr := platformCatalogBaseline(handle)
	closeErr := handle.Close()
	if err := errors.Join(baselineErr, closeErr); err != nil {
		// Delayed dehydration invalidates the pre-baseline hydrated audit; avoid
		// emitting stale hydrated attributes under decision="cloud_placeholder".
		return openedSourceHandle{}, catalog.SourceIdentity{}, catalog.VersionCandidate{}, fmt.Errorf("inspect selected root object: %w", err)
	}
	if opened.audit.hydratedCloud {
		source.traceSelectedRootPolicy(slot, "hydrated_cloud", opened.audit)
	}
	return opened, identity, candidate, nil
}

func (source *SelectedCatalogSource) buildSelectedRootRecord(
	absolute string,
	relative string,
	slot RootSlot,
	parent catalog.DirectoryID,
	kind sourceEntryKind,
	opened openedSourceHandle,
	identity catalog.SourceIdentity,
	candidate catalog.VersionCandidate,
) (catalog.NodeRecord, error) {
	locator, err := NewSourceReference(slot, filepath.ToSlash(relative))
	if err != nil {
		return catalog.NodeRecord{}, err
	}
	name := filepath.Base(absolute)
	if kind == sourceEntryDirectory {
		directory, idErr := source.newDirectoryID()
		if idErr != nil {
			return catalog.NodeRecord{}, idErr
		}
		return catalog.NewDirectoryNodeRecord(directory, parent, name, locator, identity, opened.modified)
	}
	file, err := source.newFileID()
	if err != nil {
		return catalog.NodeRecord{}, err
	}
	return catalog.NewFileNodeRecord(file, parent, name, locator, identity, candidate, opened.size, opened.modified)
}

func (source *SelectedCatalogSource) SelectedRoots() []catalog.NodeRecord {
	source.mu.RLock()
	defer source.mu.RUnlock()
	return append([]catalog.NodeRecord(nil), source.selected...)
}

func (source *SelectedCatalogSource) RevisionSource() (*RootedRevisionSource, error) {
	source.mu.RLock()
	defer source.mu.RUnlock()
	if source.closed {
		return nil, content.ErrRevisionStoreClosed
	}
	// File lookup must retain the same root authority as discovery. Reopening
	// names here could bind a replaced directory outside the selected scope.
	return newPlatformRootedRevisionSource(source.roots, source.diagnostics)
}

func (source *SelectedCatalogSource) ScanDirectory(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return catalog.ScanResult{}, err
	}
	directoryID, directoryKind := request.Directory.DirectoryID()
	if !directoryKind || directoryID.IsZero() || request.Work == nil || request.Children == nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.New("osfs: invalid catalog scan request"))
	}
	locator, err := parseSourceReference(request.Directory.SourceReference())
	if err != nil {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	source.mu.RLock()
	if source.closed || int(locator.RootSlot()) >= len(source.roots) {
		source.mu.RUnlock()
		return catalog.ScanResult{}, catalog.NewPermanentScanError(content.ErrRevisionStoreClosed)
	}
	root := source.roots[locator.RootSlot()]
	displayRoot := source.rootDisplayPaths[locator.RootSlot()]
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
	handle, err := openRootObjectNoRecall(directory, ".")
	if err != nil {
		return catalog.ScanResult{}, catalog.NewTransientScanError(err, 0)
	}
	defer handle.Close()
	beforeIdentity, beforeCandidate, err := platformCatalogBaseline(handle)
	if err != nil || !bytes.Equal(beforeIdentity.Bytes(), request.Directory.SourceIdentity().Bytes()) {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.Join(catalog.ErrDirectoryStale, err))
	}
	result, beforeEntries, err := source.enumerateCatalogChildren(
		ctx, directory, handle, locator, request, catalog.MaxDirectoryEntries,
	)
	if err != nil {
		return catalog.ScanResult{}, err
	}
	afterHandle, err := openRootObjectNoRecall(directory, ".")
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
	return catalog.ScanResult{
		OmittedCount: result.count,
		Omissions:    source.newScanOmissionReporter(request, locator, filepath.Join(displayRoot, path), result),
	}, nil
}

func (source *SelectedCatalogSource) enumerateCatalogChildren(
	ctx context.Context,
	directory *os.Root,
	handle *os.File,
	locator sourceLocation,
	request catalog.ScanRequest,
	maxEntries uint64,
) (catalogOmissions, [sha256.Size]byte, error) {
	result := catalogOmissions{}
	captureSamples := source.omissions != nil || source.diagnostics.Enabled()
	fingerprint := newCatalogDirectoryFingerprint()
	for {
		entries, readErr := handle.ReadDir(catalogEnumerationBatchSize)
		for _, entry := range entries {
			// Every enumerated entry becomes either admitted or omitted, so exceeding
			// maxEntries always fails commit validation; fail fast before consuming
			// additional ScanWork or opening child handles.
			if fingerprint.count == maxEntries {
				return catalogOmissions{}, [sha256.Size]byte{}, catalog.ErrPageLimit
			}
			fingerprint.add(entry)
			if err := request.Work.Consume(1); err != nil {
				return catalogOmissions{}, [sha256.Size]byte{}, err
			}
			child, omission, audit, err := source.scanChild(ctx, directory, handle, locator, entry)
			if err != nil {
				return catalogOmissions{}, [sha256.Size]byte{}, err
			}
			if omission.Reason != catalog.SourceOmissionNone {
				result.count++
				if captureSamples && len(result.samples) < catalog.MaxSourceOmissionSamples {
					result.samples = append(result.samples, omission)
					result.audits = append(result.audits, audit)
				}
				continue
			}
			if err := request.Children.Add(ctx, child); err != nil {
				return catalogOmissions{}, [sha256.Size]byte{}, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return result, fingerprint.sum(), nil
		}
		if readErr != nil {
			return catalogOmissions{}, [sha256.Size]byte{}, catalog.NewTransientScanError(readErr, 0)
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

func (source *SelectedCatalogSource) inspectChildHandle(name string, handle *os.File) (openedSourceHandle, error) {
	if source.inspectChildHandleFn != nil {
		return source.inspectChildHandleFn(name, handle)
	}
	return inspectOpenedSourceHandle(handle)
}

func (source *SelectedCatalogSource) scanChild(
	ctx context.Context,
	directory *os.Root,
	dirHandle *os.File,
	parentLocator sourceLocation,
	entry fs.DirEntry,
) (catalog.ScannedChild, catalog.SourceOmission, sourceEntryAudit, error) {
	if err := ctx.Err(); err != nil {
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, err
	}
	name := entry.Name()
	before, kind, beforeAudit, err := inspectRootChildKind(directory, dirHandle, entry)
	if err != nil {
		// lstat usually needs only parent search access, but policy or races can deny it.
		omission, scanErr := classifyChildAccessError(name, err)
		return catalog.ScannedChild{}, omission, sourceEntryAudit{}, scanErr
	}
	if reason := omissionForEntryKind(kind); reason != catalog.SourceOmissionNone {
		return catalog.ScannedChild{}, catalog.SourceOmission{Name: name, Reason: reason}, beforeAudit, nil
	}
	// Public names are admitted independently of the native locator. A readable
	// name can need destination repair without being excluded from the catalog.
	if _, err := catalog.CanonicalName(name); err != nil {
		return catalog.ScannedChild{}, catalog.SourceOmission{
			Name: name, Reason: catalog.SourceOmissionName, Detail: err.Error(),
		}, sourceEntryAudit{}, nil
	}
	locator, err := newChildSourceReference(parentLocator, name)
	if err != nil {
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewPermanentScanError(err)
	}
	handle, err := openRootObjectNoRecall(directory, name)
	if err != nil {
		omission, scanErr := classifyChildAccessError(name, err)
		return catalog.ScannedChild{}, omission, sourceEntryAudit{}, scanErr
	}
	opened, inspectErr := source.inspectChildHandle(name, handle)
	if inspectErr != nil {
		_ = handle.Close()
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewTransientScanError(inspectErr, 0)
	}
	sameObject := before.sameObject(opened.identity)
	if sameObject && opened.kind == sourceEntryCloudPlaceholder {
		// Only this child dehydrated before open; omit it while keeping readable siblings.
		if closeErr := handle.Close(); closeErr != nil {
			return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewTransientScanError(closeErr, 0)
		}
		return catalog.ScannedChild{}, catalog.SourceOmission{Name: name, Reason: catalog.SourceOmissionCloudPlaceholder}, opened.audit, nil
	}
	if !sameObject || opened.kind != kind {
		_ = handle.Close()
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewPermanentScanError(catalog.ErrDirectoryStale)
	}
	identity, candidate, baselineErr := platformCatalogBaseline(handle)
	closeErr := handle.Close()
	if sameObject && errors.Is(baselineErr, errSourceCloudPlaceholder) {
		if closeErr != nil {
			return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewTransientScanError(closeErr, 0)
		}
		// Do not reuse stale pre-dehydration hydrated audit fields.
		return catalog.ScannedChild{}, catalog.SourceOmission{Name: name, Reason: catalog.SourceOmissionCloudPlaceholder}, sourceEntryAudit{}, nil
	}
	if err := errors.Join(baselineErr, closeErr); err != nil {
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, catalog.NewTransientScanError(err, 0)
	}
	child := catalog.ScannedChild{Name: name, SourceReference: locator, SourceIdentity: identity, ModifiedTime: opened.modified}
	if opened.kind == sourceEntryDirectory {
		child.DirectoryID, err = source.newDirectoryID()
	} else {
		child.FileID, err = source.newFileID()
		child.VersionCandidate = candidate
		child.ExpectedSize = opened.size
	}
	if err != nil {
		return catalog.ScannedChild{}, catalog.SourceOmission{}, sourceEntryAudit{}, err
	}
	return child, catalog.SourceOmission{}, sourceEntryAudit{}, nil
}

func portableCatalogModifiedTime(information fs.FileInfo) (catalog.ModifiedTime, error) {
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
	source.rootDisplayPaths = nil
	source.mu.Unlock()
	return closeRoots(roots)
}
