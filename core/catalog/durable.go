package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	fileCatalogPagesName     = "pages"
)

var fileCatalogMagic = [4]byte{'W', 'S', 'C', '4'}

type FileBackendFaultPoint string

const (
	FileFaultStageDirectory      FileBackendFaultPoint = "stage-directory"
	FileFaultStageChild          FileBackendFaultPoint = "stage-child"
	FileFaultStagePage           FileBackendFaultPoint = "stage-page"
	FileFaultStagePageCommitment FileBackendFaultPoint = "stage-page-commitment"
	FileFaultStageNodeIndex      FileBackendFaultPoint = "stage-node-index"
	FileFaultPrepare             FileBackendFaultPoint = "prepare"
	FileFaultPublish             FileBackendFaultPoint = "publish"
	FileFaultStageFailure        FileBackendFaultPoint = "stage-failure"
	FileFaultPublishFailure      FileBackendFaultPoint = "publish-failure"
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
	nodeIndexes  map[DirectoryID]fileNodeIndex
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
	b.nodeIndexes = nil
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
	// CatalogStore admits recovered memory before the first lookup allocates
	// filters, so a large recovered share cannot allocate past its budget here.
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
	pagesPath := filepath.Join(path, fileCatalogPagesName)
	if err := os.Mkdir(pagesPath, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	children, err := os.OpenFile(filepath.Join(path, fileCatalogChildrenName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	metadata, err := os.OpenFile(filepath.Join(path, fileCatalogMetaName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = children.Close()
		_ = os.RemoveAll(path)
		return nil, err
	}
	return &fileCatalogTransaction{
		backend: b, directory: directory, generation: generation, meter: meter,
		path: path, pagesPath: pagesPath, children: children, metadata: metadata, digest: sha256.New(),
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

func (b *FileCatalogBackend) LoadPagePayload(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (PagePayload, bool, error) {
	if err := ctx.Err(); err != nil {
		return PagePayload{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return PagePayload{}, false, ErrCatalogClosed
	}
	path := b.directoryPath(directory)
	metadata, meta, err := openFileCatalogMeta(filepath.Join(path, fileCatalogMetaName))
	if errors.Is(err, os.ErrNotExist) {
		return PagePayload{}, false, nil
	}
	if err != nil {
		return PagePayload{}, false, err
	}
	defer metadata.Close()
	if meta.share != b.share || meta.directory != directory {
		return PagePayload{}, false, ErrCorruptCatalogStorage
	}
	if meta.generation != generation || index >= meta.pageCount {
		return PagePayload{}, false, nil
	}
	payload, err := readFilePagePayload(path, meta, metadata, index)
	return payload, err == nil, err
}

func readFileCatalogMeta(path string) (fileCatalogMeta, error) {
	file, meta, err := openFileCatalogMeta(path)
	if err != nil {
		return fileCatalogMeta{}, err
	}
	return meta, file.Close()
}

// Only the fixed header and the requested digest are needed to serve a page,
// even when the directory has many committed pages.
func openFileCatalogMeta(path string) (*os.File, fileCatalogMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fileCatalogMeta{}, err
	}
	var header [fileCatalogMetaBytes]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		_ = file.Close()
		return nil, fileCatalogMeta{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	meta, err := decodeFileCatalogMeta(header[:])
	if err != nil {
		_ = file.Close()
		return nil, fileCatalogMeta{}, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != filePageDigestOffset(meta.pageCount) {
		_ = file.Close()
		return nil, fileCatalogMeta{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	return file, meta, nil
}

func filePageDigestOffset(index uint32) int64 {
	return fileCatalogMetaBytes + int64(index)*sha256.Size
}

// The storage digest covers the address so that moving a payload and its digest
// together cannot grant it authority at another slot. PageCommitment remains
// the protocol digest over the canonical bytes alone.
func filePageDigest(address PageAddress, commitment PageCommitment) [sha256.Size]byte {
	var binding [3*IdentityBytes + 4 + PageCommitmentBytes]byte
	copy(binding[:IdentityBytes], address.ShareInstance[:])
	copy(binding[IdentityBytes:2*IdentityBytes], address.DirectoryID[:])
	copy(binding[2*IdentityBytes:3*IdentityBytes], address.Generation[:])
	indexEnd := 3*IdentityBytes + 4
	binary.BigEndian.PutUint32(binding[3*IdentityBytes:indexEnd], address.PageIndex)
	copy(binding[indexEnd:], commitment[:])
	return sha256.Sum256(binding[:])
}

func fileCatalogPageName(index uint32) string {
	return fmt.Sprintf("%08x.page", index)
}

func readFilePagePayload(path string, meta fileCatalogMeta, metadata io.ReaderAt, index uint32) (PagePayload, error) {
	var expected [sha256.Size]byte
	if _, err := metadata.ReadAt(expected[:], filePageDigestOffset(index)); err != nil {
		return PagePayload{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	encoded, err := readCatalogObject(filepath.Join(path, fileCatalogPagesName, fileCatalogPageName(index)))
	if err != nil {
		return PagePayload{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	if len(encoded) == 0 || len(encoded) > MaxCatalogPayloadBytes {
		return PagePayload{}, ErrCorruptCatalogStorage
	}
	address := PageAddress{ShareInstance: meta.share, DirectoryID: meta.directory, Generation: meta.generation, PageIndex: index}
	commitment := PageCommitment(sha256.Sum256(encoded))
	if filePageDigest(address, commitment) != expected || index == meta.pageCount-1 && commitment != meta.terminal {
		return PagePayload{}, ErrCorruptCatalogStorage
	}
	return PagePayload{address: address, commitment: commitment, encoded: encoded}, nil
}

func (b *FileCatalogBackend) Close() error {
	b.mu.Lock()
	b.closed = true
	b.nodeIndexes = nil
	b.mu.Unlock()
	return nil
}

func (b *FileCatalogBackend) Destroy() error {
	b.mu.Lock()
	b.closed = true
	b.nodeIndexes = nil
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
