package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sync"
)

const (
	fileCatalogMetaBytes     = 148
	maxCatalogStorageRecord  = uint64(256) << 10
	fileCatalogDirectoryName = "directory.node"
	fileCatalogChildrenName  = "children.nodes"
	fileCatalogMetaName      = "meta.bin"
	fileCatalogObjectsName   = "objects"
)

var fileCatalogMagic = [4]byte{'W', 'S', 'C', '2'}

type FileBackendFaultPoint string

const (
	FileFaultStageDirectory  FileBackendFaultPoint = "stage-directory"
	FileFaultStageChild      FileBackendFaultPoint = "stage-child"
	FileFaultStagePage       FileBackendFaultPoint = "stage-page"
	FileFaultStagePageObject FileBackendFaultPoint = "stage-page-object"
	FileFaultPrepare         FileBackendFaultPoint = "prepare"
	FileFaultPublish         FileBackendFaultPoint = "publish"
	FileFaultStageFailure    FileBackendFaultPoint = "stage-failure"
	FileFaultPublishFailure  FileBackendFaultPoint = "publish-failure"
)

type FileBackendFaults interface {
	Fail(FileBackendFaultPoint) error
}

type FileCatalogBackendConfig struct {
	Root          string
	ShareInstance ShareInstance
	Faults        FileBackendFaults
}

type FileCatalogBackend struct {
	mu           sync.RWMutex
	root         string
	committedDir string
	stagingDir   string
	failuresDir  string
	share        ShareInstance
	faults       FileBackendFaults
	closed       bool
}

func NewFileCatalogBackend(config FileCatalogBackendConfig) (*FileCatalogBackend, error) {
	if config.Root == "" || config.ShareInstance.IsZero() {
		return nil, errors.New("file catalog backend requires a root and share identity")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog backend root: %w", err)
	}
	backend := &FileCatalogBackend{
		root: root, committedDir: filepath.Join(root, "committed"),
		stagingDir: filepath.Join(root, "transactions"), failuresDir: filepath.Join(root, "failures"),
		share: config.ShareInstance, faults: config.Faults,
	}
	for _, path := range []string{backend.committedDir, backend.stagingDir, backend.failuresDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create catalog backend directory: %w", err)
		}
	}
	return backend, nil
}

func (b *FileCatalogBackend) Recover(ctx context.Context) (ResourceUsage, error) {
	if err := ctx.Err(); err != nil {
		return ResourceUsage{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ResourceUsage{}, ErrCatalogClosed
	}
	if err := os.RemoveAll(b.stagingDir); err != nil {
		return ResourceUsage{}, fmt.Errorf("clean abandoned catalog transactions: %w", err)
	}
	if err := os.MkdirAll(b.stagingDir, 0o700); err != nil {
		return ResourceUsage{}, fmt.Errorf("recreate catalog transaction directory: %w", err)
	}
	entries, err := os.ReadDir(b.committedDir)
	if err != nil {
		return ResourceUsage{}, err
	}
	var usage ResourceUsage
	for _, entry := range entries {
		if !entry.IsDir() {
			return ResourceUsage{}, fmt.Errorf("%w: unexpected committed object %q", ErrCorruptCatalogStorage, entry.Name())
		}
		if err := ctx.Err(); err != nil {
			return ResourceUsage{}, err
		}
		meta, err := b.validateCommittedPath(ctx, filepath.Join(b.committedDir, entry.Name()))
		if err != nil {
			return ResourceUsage{}, err
		}
		next, ok := addUsage(usage, meta.usage())
		if !ok {
			return ResourceUsage{}, ErrBudgetExceeded
		}
		usage = next
	}
	failureUsage, err := b.recoverFailures(ctx)
	if err != nil {
		return ResourceUsage{}, err
	}
	usage, ok := addUsage(usage, failureUsage)
	if !ok {
		return ResourceUsage{}, ErrBudgetExceeded
	}
	return usage, nil
}

func (b *FileCatalogBackend) BeginDirectory(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, meter ResourceMeter) (BackendTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory.IsZero() || generation.IsZero() || meter == nil {
		return nil, errors.New("catalog backend transaction requires identities and a staging meter")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return nil, ErrCatalogClosed
	}
	path, err := os.MkdirTemp(b.stagingDir, hex.EncodeToString(directory[:])+"-")
	if err != nil {
		return nil, fmt.Errorf("create catalog transaction: %w", err)
	}
	pagesPath := filepath.Join(path, "pages")
	if err := os.Mkdir(pagesPath, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	objectsPath := filepath.Join(path, fileCatalogObjectsName)
	if err := os.Mkdir(objectsPath, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	children, err := os.OpenFile(filepath.Join(path, fileCatalogChildrenName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	return &fileCatalogTransaction{
		backend: b, directory: directory, generation: generation, meter: meter,
		path: path, pagesPath: pagesPath, objectsPath: objectsPath, children: children, digest: sha256.New(),
	}, nil
}

func (b *FileCatalogBackend) LoadDirectory(ctx context.Context, directory DirectoryID) (CommittedDirectory, bool, error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return CommittedDirectory{}, false, ErrCatalogClosed
	}
	meta, err := readFileCatalogMeta(filepath.Join(b.directoryPath(directory), fileCatalogMetaName))
	if errors.Is(err, os.ErrNotExist) {
		return CommittedDirectory{}, false, nil
	}
	if err != nil {
		return CommittedDirectory{}, false, err
	}
	if meta.share != b.share || meta.directory != directory {
		return CommittedDirectory{}, false, ErrCorruptCatalogStorage
	}
	return meta.committed(), true, nil
}

func (b *FileCatalogBackend) LoadPage(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return CatalogPage{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return CatalogPage{}, false, ErrCatalogClosed
	}
	meta, err := readFileCatalogMeta(filepath.Join(b.directoryPath(directory), fileCatalogMetaName))
	if errors.Is(err, os.ErrNotExist) || err == nil && (meta.generation != generation || index >= meta.pageCount) {
		return CatalogPage{}, false, nil
	}
	if err != nil {
		return CatalogPage{}, false, err
	}
	encoded, err := readCatalogObject(b.pagePath(directory, index))
	if err != nil {
		return CatalogPage{}, false, err
	}
	page, err := decodeCatalogPage(encoded)
	if err != nil {
		return CatalogPage{}, false, err
	}
	if page.DirectoryID() != directory || page.Generation() != generation || page.PageIndex() != index {
		return CatalogPage{}, false, ErrCorruptCatalogStorage
	}
	return page, true, nil
}

func (b *FileCatalogBackend) LoadPageObject(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (SealedPageObject, bool, error) {
	if err := ctx.Err(); err != nil {
		return SealedPageObject{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return SealedPageObject{}, false, ErrCatalogClosed
	}
	meta, err := readFileCatalogMeta(filepath.Join(b.directoryPath(directory), fileCatalogMetaName))
	if errors.Is(err, os.ErrNotExist) || err == nil && (meta.generation != generation || index >= meta.pageCount) {
		return SealedPageObject{}, false, nil
	}
	if err != nil {
		return SealedPageObject{}, false, err
	}
	encoded, err := readCatalogObject(b.pageObjectPath(directory, index))
	if err != nil {
		return SealedPageObject{}, false, err
	}
	object, err := NewSealedPageObject(encoded)
	if err != nil {
		return SealedPageObject{}, false, errors.Join(ErrCorruptCatalogStorage, err)
	}
	pageBytes, err := readCatalogObject(b.pagePath(directory, index))
	if err != nil {
		return SealedPageObject{}, false, err
	}
	page, err := decodeCatalogPage(pageBytes)
	if err != nil || page.DirectoryID() != directory || page.Generation() != generation || page.PageIndex() != index {
		return SealedPageObject{}, false, ErrCorruptCatalogStorage
	}
	if object.Commitment() != page.Commitment() {
		return SealedPageObject{}, false, ErrCorruptCatalogStorage
	}
	return object, true, nil
}

func (b *FileCatalogBackend) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func (b *FileCatalogBackend) Destroy() error {
	b.mu.Lock()
	b.closed = true
	root := b.root
	b.mu.Unlock()
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("destroy catalog backend: %w", err)
	}
	return nil
}

func (b *FileCatalogBackend) pagePath(directory DirectoryID, index uint32) string {
	return filepath.Join(b.directoryPath(directory), "pages", fmt.Sprintf("%08x.page", index))
}

func (b *FileCatalogBackend) pageObjectPath(directory DirectoryID, index uint32) string {
	return filepath.Join(b.directoryPath(directory), fileCatalogObjectsName, fmt.Sprintf("%08x.object", index))
}

type fileCatalogTransaction struct {
	mu              sync.Mutex
	backend         *FileCatalogBackend
	directory       DirectoryID
	generation      DirectoryGeneration
	meter           ResourceMeter
	path            string
	pagesPath       string
	objectsPath     string
	children        *os.File
	directoryRecord NodeRecord
	pending         []NodeRecord
	pendingMemory   uint64
	sequence        directorySequence
	digest          hash.Hash
	stagedBytes     uint64
	preparation     BackendPreparation
	preparedMeta    fileCatalogMeta
	prepared        bool
	finished        bool
}

func (t *fileCatalogTransaction) ensureOpen() error {
	if t.finished {
		return errors.New("catalog backend transaction is already finished")
	}
	return nil
}

func (t *fileCatalogTransaction) fault(point FileBackendFaultPoint) error {
	if t.backend.faults == nil {
		return nil
	}
	return t.backend.faults.Fail(point)
}

func (t *fileCatalogTransaction) stage(path string, encoded []byte, point FileBackendFaultPoint) error {
	if err := t.fault(point); err != nil {
		return err
	}
	if err := t.meter.Consume(ResourceUsage{SpillBytes: uint64(len(encoded))}); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := writeFull(file, encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	t.stagedBytes += uint64(len(encoded))
	return nil
}

func (t *fileCatalogTransaction) PutDirectory(record NodeRecord) (resultErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	directory, ok := record.DirectoryID()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if t.prepared || !record.valid() || !ok || directory != t.directory || t.directoryRecord.valid() {
		return errors.New("catalog transaction directory record does not match its target")
	}
	temporary := ResourceUsage{MemoryBytes: record.EstimatedMemoryBytes()}
	if err := t.meter.Consume(temporary); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, t.meter.Release(temporary))
	}()
	encoded, err := encodeNodeRecord(record)
	if err != nil {
		return err
	}
	if err := t.stage(filepath.Join(t.path, fileCatalogDirectoryName), encoded, FileFaultStageDirectory); err != nil {
		return err
	}
	t.directoryRecord = record
	hashCatalogObject(t.digest, 1, encoded)
	return nil
}

func (t *fileCatalogTransaction) PutChild(record NodeRecord) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if t.prepared || !record.valid() || record.Parent() != t.directory || len(t.pending) >= MaxCatalogPageEntries {
		return errors.New("catalog transaction child is invalid, unordered, or has another parent")
	}
	pendingMemory := record.EstimatedMemoryBytes()
	if err := t.meter.Consume(ResourceUsage{Entries: 1, MemoryBytes: pendingMemory}); err != nil {
		return err
	}
	encoded, err := encodeNodeRecord(record)
	if err != nil {
		return err
	}
	if err := t.fault(FileFaultStageChild); err != nil {
		return err
	}
	if uint64(len(encoded)) > uint64(^uint32(0)) {
		return errors.New("catalog node exceeds durable framing")
	}
	frameBytes := uint64(4 + len(encoded))
	if err := t.meter.Consume(ResourceUsage{SpillBytes: frameBytes}); err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	if err := writeFull(t.children, header[:]); err != nil {
		return err
	}
	if err := writeFull(t.children, encoded); err != nil {
		return err
	}
	t.stagedBytes += frameBytes
	t.pendingMemory += pendingMemory
	t.pending = append(t.pending, record)
	hashCatalogObject(t.digest, 2, encoded)
	return nil
}

func (t *fileCatalogTransaction) PutPage(page CatalogPage, object SealedPageObject) (resultErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if t.prepared || page.DirectoryID() != t.directory || page.Generation() != t.generation ||
		len(t.pending) != len(page.entries) || object.IsZero() || object.Commitment() != page.Commitment() {
		return errors.New("catalog transaction page does not match its target or private records")
	}
	for index, record := range t.pending {
		if !record.MatchesEntry(page.entries[index]) {
			return errors.New("catalog transaction page changed a private child record")
		}
	}
	if err := t.sequence.accept(page); err != nil {
		return err
	}
	temporary := ResourceUsage{MemoryBytes: page.EstimatedMemoryBytes()}
	if err := t.meter.Consume(temporary); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, t.meter.Release(temporary))
	}()
	encoded, err := encodeCatalogPage(page)
	if err != nil {
		return err
	}
	path := filepath.Join(t.pagesPath, fmt.Sprintf("%08x.page", page.pageIndex))
	if err := t.stage(path, encoded, FileFaultStagePage); err != nil {
		return err
	}
	objectBytes := object.Bytes()
	objectPath := filepath.Join(t.objectsPath, fmt.Sprintf("%08x.object", page.pageIndex))
	if err := t.stage(objectPath, objectBytes, FileFaultStagePageObject); err != nil {
		return err
	}
	if err := t.meter.Release(ResourceUsage{MemoryBytes: t.pendingMemory}); err != nil {
		return err
	}
	t.pendingMemory = 0
	t.pending = t.pending[:0]
	hashCatalogObject(t.digest, 3, encoded)
	hashCatalogObject(t.digest, 4, objectBytes)
	return nil
}

func (t *fileCatalogTransaction) Prepare(ctx context.Context) (BackendPreparation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return BackendPreparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackendPreparation{}, err
	}
	if t.prepared {
		return t.preparation, nil
	}
	if err := t.fault(FileFaultPrepare); err != nil {
		return BackendPreparation{}, err
	}
	if !t.directoryRecord.valid() || len(t.pending) != 0 {
		return BackendPreparation{}, errors.New("catalog transaction is incomplete")
	}
	committed, err := t.sequence.finish()
	if err != nil {
		return BackendPreparation{}, err
	}
	if err := t.children.Sync(); err != nil {
		return BackendPreparation{}, err
	}
	if err := t.children.Close(); err != nil {
		return BackendPreparation{}, err
	}
	t.children = nil
	var digest [sha256.Size]byte
	copy(digest[:], t.digest.Sum(nil))
	meta := fileCatalogMeta{
		share: committed.shareInstance, directory: committed.directoryID, generation: committed.generation,
		pageCount: committed.pageCount, entryCount: committed.entryCount, omitted: committed.omittedCount,
		terminal: committed.terminalCommitment, digest: digest, spillBytes: t.stagedBytes + fileCatalogMetaBytes,
	}
	if err := t.stage(filepath.Join(t.path, fileCatalogMetaName), encodeFileCatalogMeta(meta), FileFaultPrepare); err != nil {
		return BackendPreparation{}, err
	}
	existing, exists, err := t.existingMeta()
	if err != nil {
		return BackendPreparation{}, err
	}
	if exists && (existing.digest != meta.digest || existing.committed() != committed) {
		return BackendPreparation{}, ErrGenerationConflict
	}
	if !exists {
		if err := t.rejectForeignNodeCollisions(ctx); err != nil {
			return BackendPreparation{}, err
		}
	}
	t.preparation = BackendPreparation{Directory: committed, Usage: meta.usage(), Existing: exists}
	t.preparedMeta = meta
	t.prepared = true
	return t.preparation, nil
}

func (t *fileCatalogTransaction) existingMeta() (fileCatalogMeta, bool, error) {
	meta, err := readFileCatalogMeta(filepath.Join(t.backend.directoryPath(t.directory), fileCatalogMetaName))
	if errors.Is(err, os.ErrNotExist) {
		return fileCatalogMeta{}, false, nil
	}
	return meta, err == nil, err
}

func (t *fileCatalogTransaction) rejectForeignNodeCollisions(ctx context.Context) error {
	t.backend.mu.RLock()
	defer t.backend.mu.RUnlock()
	if existing, found, err := t.backend.loadNodeLocked(ctx, t.directoryRecord.NodeID(), t.directory); err != nil {
		return err
	} else if found && existing != t.directoryRecord {
		return ErrGenerationConflict
	}
	children, err := os.Open(filepath.Join(t.path, fileCatalogChildrenName))
	if err != nil {
		return err
	}
	defer children.Close()
	for {
		encoded, ok, err := readNodeFrame(children)
		if err != nil || !ok {
			return err
		}
		record, err := decodeNodeRecord(encoded)
		if err != nil {
			return err
		}
		if _, found, err := t.backend.loadNodeLocked(ctx, record.NodeID(), t.directory); err != nil {
			return err
		} else if found {
			return ErrGenerationConflict
		}
	}
}

func (t *fileCatalogTransaction) Publish(ctx context.Context) (CommittedDirectory, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return CommittedDirectory{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, err
	}
	if !t.prepared {
		return CommittedDirectory{}, errors.New("catalog transaction must be prepared before publication")
	}
	if t.preparation.Existing {
		t.finished = true
		_ = os.RemoveAll(t.path)
		return t.preparation.Directory, nil
	}
	if err := t.fault(FileFaultPublish); err != nil {
		return CommittedDirectory{}, err
	}
	t.backend.mu.Lock()
	defer t.backend.mu.Unlock()
	if t.backend.closed {
		return CommittedDirectory{}, ErrCatalogClosed
	}
	target := t.backend.directoryPath(t.directory)
	if _, err := os.Stat(target); err == nil {
		meta, readErr := readFileCatalogMeta(filepath.Join(target, fileCatalogMetaName))
		if readErr != nil || meta.committed() != t.preparation.Directory || meta.digest != t.preparedMeta.digest {
			return CommittedDirectory{}, ErrGenerationConflict
		}
		t.finished = true
		_ = os.RemoveAll(t.path)
		return meta.committed(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return CommittedDirectory{}, err
	}
	// No reader can observe target while the backend write lock is held. That
	// lets a failed parent-directory sync roll the rename back without exposing a
	// generation which the caller was told had failed.
	for _, path := range []string{t.pagesPath, t.objectsPath, t.path} {
		if err := syncCatalogDirectory(path); err != nil {
			return CommittedDirectory{}, fmt.Errorf("sync catalog transaction directory: %w", err)
		}
	}
	if err := os.Rename(t.path, target); err != nil {
		return CommittedDirectory{}, fmt.Errorf("publish catalog generation: %w", err)
	}
	if err := syncCatalogDirectory(t.backend.committedDir); err != nil {
		if rollbackErr := os.Rename(target, t.path); rollbackErr == nil {
			_ = syncCatalogDirectory(t.backend.committedDir)
			return CommittedDirectory{}, fmt.Errorf("sync catalog publication: %w", err)
		}
		// A complete target is already the authoritative state if the namespace
		// cannot be rolled back; returning success keeps budget and visibility in
		// agreement, and recovery will validate or reject it after a crash.
	}
	t.finished = true
	return t.preparation.Directory, nil
}

func (t *fileCatalogTransaction) Abort() error {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return nil
	}
	t.finished = true
	children := t.children
	t.children = nil
	path := t.path
	t.mu.Unlock()
	if children != nil {
		_ = children.Close()
	}
	return os.RemoveAll(path)
}
