package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// CatalogSpillRoot keeps committed pages and their transient sort runs under the
// same lifecycle-owned storage authority without exposing backend internals.
func (b *FileCatalogBackend) CatalogSpillRoot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return filepath.Join(b.root, "sort")
}

func (b *FileCatalogBackend) pagePath(directory DirectoryID, index uint32) string {
	return filepath.Join(b.directoryPath(directory), "pages", fmt.Sprintf("%08x.page", index))
}

func (b *FileCatalogBackend) pageObjectPath(directory DirectoryID, index uint32) string {
	return filepath.Join(b.directoryPath(directory), fileCatalogObjectsName, fmt.Sprintf("%08x.object", index))
}
