package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

type fileCatalogMeta struct {
	share      ShareInstance
	directory  DirectoryID
	generation DirectoryGeneration
	pageCount  uint32
	entryCount uint64
	omitted    uint64
	terminal   PageCommitment
	digest     [sha256.Size]byte
	spillBytes uint64
}

func (b *FileCatalogBackend) directoryPath(directory DirectoryID) string {
	return filepath.Join(b.committedDir, hex.EncodeToString(directory[:]))
}

func (m fileCatalogMeta) committed() CommittedDirectory {
	return CommittedDirectory{
		shareInstance: m.share, directoryID: m.directory, generation: m.generation,
		pageCount: m.pageCount, entryCount: m.entryCount, omittedCount: m.omitted,
		terminalCommitment: m.terminal,
	}
}

func (m fileCatalogMeta) usage() ResourceUsage {
	return ResourceUsage{Entries: m.entryCount, SpillBytes: m.spillBytes}
}

func encodeFileCatalogMeta(meta fileCatalogMeta) []byte {
	encoded := make([]byte, fileCatalogMetaBytes)
	copy(encoded[0:4], fileCatalogMagic[:])
	binary.BigEndian.PutUint16(encoded[4:6], catalogStorageSchema)
	copy(encoded[8:24], meta.share[:])
	copy(encoded[24:40], meta.directory[:])
	copy(encoded[40:56], meta.generation[:])
	binary.BigEndian.PutUint32(encoded[56:60], meta.pageCount)
	binary.BigEndian.PutUint64(encoded[60:68], meta.entryCount)
	binary.BigEndian.PutUint64(encoded[68:76], meta.omitted)
	copy(encoded[76:108], meta.terminal[:])
	copy(encoded[108:140], meta.digest[:])
	binary.BigEndian.PutUint64(encoded[140:148], meta.spillBytes)
	return encoded
}

func decodeFileCatalogMeta(encoded []byte) (fileCatalogMeta, error) {
	if len(encoded) != fileCatalogMetaBytes || !equalEncoded(encoded[0:4], fileCatalogMagic[:]) ||
		binary.BigEndian.Uint16(encoded[4:6]) != catalogStorageSchema || encoded[6] != 0 || encoded[7] != 0 {
		return fileCatalogMeta{}, ErrCorruptCatalogStorage
	}
	var meta fileCatalogMeta
	copy(meta.share[:], encoded[8:24])
	copy(meta.directory[:], encoded[24:40])
	copy(meta.generation[:], encoded[40:56])
	meta.pageCount = binary.BigEndian.Uint32(encoded[56:60])
	meta.entryCount = binary.BigEndian.Uint64(encoded[60:68])
	meta.omitted = binary.BigEndian.Uint64(encoded[68:76])
	copy(meta.terminal[:], encoded[76:108])
	copy(meta.digest[:], encoded[108:140])
	meta.spillBytes = binary.BigEndian.Uint64(encoded[140:148])
	if meta.share.IsZero() || meta.directory.IsZero() || meta.generation.IsZero() ||
		meta.pageCount == 0 || meta.terminal.IsZero() || meta.entryCount > MaxDirectoryEntries ||
		meta.pageCount != catalogPageCount(meta.entryCount) ||
		meta.omitted > MaxDirectoryEntries-meta.entryCount {
		return fileCatalogMeta{}, ErrCorruptCatalogStorage
	}
	return meta, nil
}

func catalogPageCount(entries uint64) uint32 {
	if entries == 0 {
		return 1
	}
	return uint32((entries + MaxCatalogPageEntries - 1) / MaxCatalogPageEntries)
}

func readFileCatalogMeta(path string) (fileCatalogMeta, error) {
	encoded, err := readCatalogObject(path)
	if err != nil {
		return fileCatalogMeta{}, err
	}
	return decodeFileCatalogMeta(encoded)
}

func (b *FileCatalogBackend) LoadNode(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return NodeRecord{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return NodeRecord{}, false, ErrCatalogClosed
	}
	return b.loadNodeLocked(ctx, id, DirectoryID{})
}

func (b *FileCatalogBackend) loadNodeLocked(ctx context.Context, id NodeID, exclude DirectoryID) (NodeRecord, bool, error) {
	directories, err := os.ReadDir(b.committedDir)
	if err != nil {
		return NodeRecord{}, false, err
	}
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return NodeRecord{}, false, err
		}
		meta, err := readFileCatalogMeta(filepath.Join(b.committedDir, directory.Name(), fileCatalogMetaName))
		if err != nil {
			return NodeRecord{}, false, err
		}
		if meta.directory == exclude {
			continue
		}
		path := filepath.Join(b.committedDir, directory.Name())
		encoded, err := readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
		if err != nil {
			return NodeRecord{}, false, err
		}
		record, err := decodeNodeRecord(encoded)
		if err != nil {
			return NodeRecord{}, false, err
		}
		if record.NodeID() == id {
			return record, true, nil
		}
		children, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
		if err != nil {
			return NodeRecord{}, false, err
		}
		record, found, scanErr := findNodeInFile(ctx, children, id)
		closeErr := children.Close()
		if scanErr != nil {
			return NodeRecord{}, false, scanErr
		}
		if closeErr != nil {
			return NodeRecord{}, false, closeErr
		}
		if found {
			return record, true, nil
		}
	}
	return NodeRecord{}, false, nil
}

func (b *FileCatalogBackend) validateCommittedPath(ctx context.Context, path string) (fileCatalogMeta, error) {
	meta, directoryBytes, err := b.readCommittedIdentity(path)
	if err != nil {
		return fileCatalogMeta{}, err
	}
	children, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
	if err != nil {
		return fileCatalogMeta{}, err
	}
	computed, validationErr := validateCommittedContents(ctx, path, meta, directoryBytes, children)
	closeErr := children.Close()
	if err := errors.Join(validationErr, closeErr); err != nil {
		return fileCatalogMeta{}, err
	}
	if computed != meta.digest {
		return fileCatalogMeta{}, ErrCorruptCatalogStorage
	}
	size, err := directoryTreeBytes(path)
	if err != nil || size != meta.spillBytes {
		return fileCatalogMeta{}, ErrCorruptCatalogStorage
	}
	return meta, nil
}

func (b *FileCatalogBackend) readCommittedIdentity(path string) (fileCatalogMeta, []byte, error) {
	meta, err := readFileCatalogMeta(filepath.Join(path, fileCatalogMetaName))
	if err != nil {
		return fileCatalogMeta{}, nil, err
	}
	if meta.share != b.share || filepath.Base(path) != hex.EncodeToString(meta.directory[:]) {
		return fileCatalogMeta{}, nil, ErrCorruptCatalogStorage
	}
	directoryBytes, err := readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
	if err != nil {
		return fileCatalogMeta{}, nil, err
	}
	directory, err := decodeNodeRecord(directoryBytes)
	if err != nil {
		return fileCatalogMeta{}, nil, err
	}
	if id, ok := directory.DirectoryID(); !ok || id != meta.directory {
		return fileCatalogMeta{}, nil, ErrCorruptCatalogStorage
	}
	return meta, directoryBytes, nil
}

func validateCommittedContents(
	ctx context.Context,
	path string,
	meta fileCatalogMeta,
	directoryBytes []byte,
	children io.Reader,
) ([sha256.Size]byte, error) {
	digest := sha256.New()
	hashCatalogObject(digest, 1, directoryBytes)
	var sequence directorySequence
	for index := uint32(0); index < meta.pageCount; index++ {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		if err := validateCommittedPage(path, meta.directory, index, children, digest, &sequence); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	if _, ok, err := readNodeFrame(children); err != nil || ok {
		return [sha256.Size]byte{}, ErrCorruptCatalogStorage
	}
	committed, err := sequence.finish()
	if err != nil || committed != meta.committed() {
		return [sha256.Size]byte{}, ErrCorruptCatalogStorage
	}
	var computed [sha256.Size]byte
	copy(computed[:], digest.Sum(nil))
	return computed, nil
}

func validateCommittedPage(
	path string,
	directory DirectoryID,
	index uint32,
	children io.Reader,
	digest hash.Hash,
	sequence *directorySequence,
) error {
	pageBytes, err := readCatalogObject(filepath.Join(path, "pages", fmt.Sprintf("%08x.page", index)))
	if err != nil {
		return err
	}
	page, err := decodeCatalogPage(pageBytes)
	if err != nil {
		return err
	}
	objectBytes, err := readCatalogObject(filepath.Join(path, fileCatalogObjectsName, fmt.Sprintf("%08x.object", index)))
	if err != nil {
		return err
	}
	object, err := NewSealedPageObject(objectBytes)
	if err != nil || object.Commitment() != page.Commitment() {
		return ErrCorruptCatalogStorage
	}
	for _, entry := range page.entries {
		nodeBytes, ok, err := readNodeFrame(children)
		if err != nil || !ok {
			return ErrCorruptCatalogStorage
		}
		node, err := decodeNodeRecord(nodeBytes)
		if err != nil || !node.MatchesEntry(entry) || node.Parent() != directory {
			return ErrCorruptCatalogStorage
		}
		hashCatalogObject(digest, 2, nodeBytes)
	}
	if err := sequence.accept(page); err != nil {
		return err
	}
	hashCatalogObject(digest, 3, pageBytes)
	hashCatalogObject(digest, 4, objectBytes)
	return nil
}

func readCatalogObject(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Size() < 0 || uint64(info.Size()) > maxCatalogStorageRecord {
		_ = file.Close()
		return nil, ErrCorruptCatalogStorage
	}
	encoded := make([]byte, info.Size())
	_, readErr := io.ReadFull(file, encoded)
	var trailing [1]byte
	trailingBytes, trailingErr := file.Read(trailing[:])
	closeErr := file.Close()
	if readErr != nil || trailingBytes != 0 || trailingErr != nil && !errors.Is(trailingErr, io.EOF) || closeErr != nil {
		return nil, errors.Join(ErrCorruptCatalogStorage, readErr, trailingErr, closeErr)
	}
	return encoded, nil
}

func readNodeFrame(reader io.Reader) ([]byte, bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, false, nil
		}
		return nil, false, err
	}
	size := uint64(binary.BigEndian.Uint32(header[:]))
	if size == 0 || size > maxCatalogStorageRecord {
		return nil, false, ErrCorruptCatalogStorage
	}
	encoded := make([]byte, size)
	if _, err := io.ReadFull(reader, encoded); err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func findNodeInFile(ctx context.Context, reader io.Reader, id NodeID) (NodeRecord, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return NodeRecord{}, false, err
		}
		encoded, ok, err := readNodeFrame(reader)
		if err != nil || !ok {
			return NodeRecord{}, false, err
		}
		record, err := decodeNodeRecord(encoded)
		if err != nil {
			return NodeRecord{}, false, err
		}
		if record.NodeID() == id {
			return record, true, nil
		}
	}
}

func directoryTreeBytes(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || total > ^uint64(0)-uint64(info.Size()) {
			return ErrCorruptCatalogStorage
		}
		total += uint64(info.Size())
		return nil
	})
	return total, err
}

func syncCatalogDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	// Windows cannot provide a portable unprivileged directory-flush contract;
	// files and atomic rename still provide the documented process-restart level.
	if runtime.GOOS == "windows" {
		syncErr = nil
	}
	return errors.Join(syncErr, closeErr)
}
