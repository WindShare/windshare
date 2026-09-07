package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	fileCatalogNodeIndexName = "nodes.index"
	nodeIndexSlotBytes       = IdentityBytes + 8
	// Sixteen bits per node keep false positives rare without retaining IDs.
	nodeIndexBitsPerNode     = 16
	nodeIndexHashCount       = 7
	nodeIndexFilterWordBits  = 64
	nodeIndexCapacityFactor  = 2
	nodeIndexDirectoryOffset = uint64(1)
	nodeIndexChildOffsetBase = uint64(2)
	// The filter is retained; the exact table and private records stay on disk.
	// Include the map entry, slice header, and allocator overhead in the budget.
	nodeIndexMemoryOverhead = uint64(128)
)

type fileNodeIndex struct {
	filter []byte
	slots  uint64
}

func nodeIndexGeometry(entries uint64) (filterBytes, slots uint64) {
	nodes := entries + 1
	filterBytes = ((nodes*nodeIndexBitsPerNode + nodeIndexFilterWordBits - 1) / nodeIndexFilterWordBits) * (nodeIndexFilterWordBits / 8)
	slots = nodeIndexCapacityFactor
	for slots < nodes*nodeIndexCapacityFactor {
		slots *= nodeIndexCapacityFactor
	}
	return filterBytes, slots
}

func nodeIndexMemoryBytes(entries uint64) uint64 {
	filterBytes, _ := nodeIndexGeometry(entries)
	return filterBytes + nodeIndexMemoryOverhead
}

func (index fileNodeIndex) diskBytes() uint64 {
	return uint64(len(index.filter)) + index.slots*nodeIndexSlotBytes
}

func nodeIndexHash(id NodeID) [sha256.Size]byte { return sha256.Sum256(id[:]) }

func (index fileNodeIndex) filterContains(digest [sha256.Size]byte) bool {
	first := binary.LittleEndian.Uint64(digest[8:16])
	step := binary.LittleEndian.Uint64(digest[16:24]) | 1
	bits := uint64(len(index.filter)) * 8
	for probe := range uint64(nodeIndexHashCount) {
		bit := (first + probe*step) % bits
		if index.filter[bit/8]&(1<<(bit%8)) == 0 {
			return false
		}
	}
	return true
}

func (index fileNodeIndex) addFilter(digest [sha256.Size]byte) {
	first := binary.LittleEndian.Uint64(digest[8:16])
	step := binary.LittleEndian.Uint64(digest[16:24]) | 1
	bits := uint64(len(index.filter)) * 8
	for probe := range uint64(nodeIndexHashCount) {
		bit := (first + probe*step) % bits
		index.filter[bit/8] |= 1 << (bit % 8)
	}
}

func (index fileNodeIndex) findSlot(ctx context.Context, file io.ReaderAt, id NodeID, digest [sha256.Size]byte) (int64, uint64, error) {
	slot := binary.LittleEndian.Uint64(digest[:8]) & (index.slots - 1)
	var encoded [nodeIndexSlotBytes]byte
	for range index.slots {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		position := int64(uint64(len(index.filter)) + slot*nodeIndexSlotBytes)
		if _, err := file.ReadAt(encoded[:], position); err != nil {
			return 0, 0, errors.Join(ErrCorruptCatalogStorage, err)
		}
		offset := binary.BigEndian.Uint64(encoded[IdentityBytes:])
		if offset == 0 || string(encoded[:IdentityBytes]) == string(id[:]) {
			return position, offset, nil
		}
		slot = (slot + 1) & (index.slots - 1)
	}
	return 0, 0, ErrCorruptCatalogStorage
}

func readFileNodeIndex(path string, entries uint64) (fileNodeIndex, error) {
	filterBytes, slots := nodeIndexGeometry(entries)
	index := fileNodeIndex{filter: make([]byte, filterBytes), slots: slots}
	file, err := os.Open(filepath.Join(path, fileCatalogNodeIndexName))
	if err != nil {
		return fileNodeIndex{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileNodeIndex{}, err
	}
	if info.Size() != int64(index.diskBytes()) {
		return fileNodeIndex{}, ErrCorruptCatalogStorage
	}
	if _, err := io.ReadFull(file, index.filter); err != nil {
		return fileNodeIndex{}, errors.Join(ErrCorruptCatalogStorage, err)
	}
	return index, nil
}

// Each immutable generation owns its exact hash table. Small membership filters
// prevent a lookup from opening unrelated generations, while false positives
// still go through the exact table and record identity validation.
func (b *FileCatalogBackend) loadIndexedNode(ctx context.Context, directory DirectoryID, index fileNodeIndex, id NodeID, digest [sha256.Size]byte) (NodeRecord, bool, error) {
	path := b.directoryPath(directory)
	file, err := os.Open(filepath.Join(path, fileCatalogNodeIndexName))
	if err != nil {
		return NodeRecord{}, false, err
	}
	_, offset, readErr := index.findSlot(ctx, file, id, digest)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return NodeRecord{}, false, err
	}
	if offset == 0 {
		return NodeRecord{}, false, nil
	}
	meta, err := readFileCatalogMeta(filepath.Join(path, fileCatalogMetaName))
	if err != nil {
		return NodeRecord{}, false, err
	}
	if meta.share != b.share || meta.directory != directory {
		return NodeRecord{}, false, ErrCorruptCatalogStorage
	}
	var encoded []byte
	if offset == nodeIndexDirectoryOffset {
		encoded, err = readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
	} else {
		encoded, err = readIndexedChild(path, offset-nodeIndexChildOffsetBase)
	}
	if err != nil {
		return NodeRecord{}, false, err
	}
	record, err := decodeNodeRecord(encoded)
	if err != nil {
		return NodeRecord{}, false, err
	}
	if record.NodeID() != id {
		return NodeRecord{}, false, ErrCorruptCatalogStorage
	}
	return record, true, nil
}

func readIndexedChild(path string, offset uint64) ([]byte, error) {
	children, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
	if err != nil {
		return nil, err
	}
	defer children.Close()
	info, err := children.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || offset >= uint64(info.Size()) {
		return nil, ErrCorruptCatalogStorage
	}
	encoded, found, err := readNodeFrame(io.NewSectionReader(children, int64(offset), info.Size()-int64(offset)))
	if err != nil || !found {
		return nil, errors.Join(ErrCorruptCatalogStorage, err)
	}
	return encoded, nil
}

func (b *FileCatalogBackend) ensureNodeIndexes(ctx context.Context) error {
	b.mu.RLock()
	ready := b.nodeIndexes != nil
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrCatalogClosed
	}
	if ready {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrCatalogClosed
	}
	if b.nodeIndexes != nil {
		return nil
	}
	entries, err := os.ReadDir(b.committedDir)
	if err != nil {
		return err
	}
	indexes := make(map[DirectoryID]fileNodeIndex, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(b.committedDir, entry.Name())
		meta, err := b.validateCommittedPath(ctx, path)
		if err != nil {
			return err
		}
		index, err := readFileNodeIndex(path, meta.entryCount)
		if err != nil {
			return err
		}
		indexes[meta.directory] = index
	}
	b.nodeIndexes = indexes
	return nil
}
