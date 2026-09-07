package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
)

const (
	nodeIndexDigestKind        = byte(5)
	nodeIndexDigestBufferBytes = 4096
)

// Build in the unpublished generation so the existing directory rename remains
// the sole visibility boundary. No global index needs a second durable commit.
func (t *fileCatalogTransaction) buildNodeIndex(ctx context.Context, entries uint64) (resultErr error) {
	if err := t.fault(FileFaultStageNodeIndex); err != nil {
		return err
	}
	filterBytes, slots := nodeIndexGeometry(entries)
	index := fileNodeIndex{slots: slots}
	diskBytes := filterBytes + slots*nodeIndexSlotBytes
	if err := t.meter.Consume(ResourceUsage{MemoryBytes: nodeIndexMemoryBytes(entries), SpillBytes: diskBytes}); err != nil {
		return err
	}
	index.filter = make([]byte, filterBytes)
	path := filepath.Join(t.path, fileCatalogNodeIndexName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := file.Truncate(int64(diskBytes)); err != nil {
		return err
	}
	insert := func(id NodeID, offset uint64) error {
		digest := nodeIndexHash(id)
		position, existing, err := index.findSlot(ctx, file, id, digest)
		if err != nil {
			return err
		}
		if existing != 0 {
			return ErrGenerationConflict
		}
		var encoded [nodeIndexSlotBytes]byte
		copy(encoded[:IdentityBytes], id[:])
		binary.BigEndian.PutUint64(encoded[IdentityBytes:], offset)
		if _, err := file.WriteAt(encoded[:], position); err != nil {
			return err
		}
		index.addFilter(digest)
		return nil
	}
	if err := insert(t.directoryRecord.NodeID(), nodeIndexDirectoryOffset); err != nil {
		return err
	}
	children, err := os.Open(filepath.Join(t.path, fileCatalogChildrenName))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, children.Close()) }()
	var offset, count uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		encoded, found, err := readNodeFrame(children)
		if err != nil {
			return err
		}
		if !found {
			break
		}
		record, err := decodeNodeRecord(encoded)
		if err != nil {
			return err
		}
		if err := insert(record.NodeID(), offset+nodeIndexChildOffsetBase); err != nil {
			return err
		}
		offset += uint64(4 + len(encoded))
		count++
	}
	if count != entries {
		return ErrCorruptCatalogStorage
	}
	if _, err := file.WriteAt(index.filter, 0); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := hashFileNodeIndex(ctx, t.digest, path, diskBytes); err != nil {
		return err
	}
	t.stagedBytes += diskBytes
	t.nodeIndex = index
	return nil
}

func hashFileNodeIndex(ctx context.Context, digest hash.Hash, path string, diskBytes uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != int64(diskBytes) {
		return ErrCorruptCatalogStorage
	}
	var header [9]byte
	header[0] = nodeIndexDigestKind
	binary.BigEndian.PutUint64(header[1:], diskBytes)
	_, _ = digest.Write(header[:])
	// A fixed buffer bounds recovery memory even for a maximum-width generation.
	var buffer [nodeIndexDigestBufferBytes]byte
	for remaining := diskBytes; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(remaining, uint64(len(buffer)))
		if _, err := io.ReadFull(file, buffer[:count]); err != nil {
			return errors.Join(ErrCorruptCatalogStorage, err)
		}
		_, _ = digest.Write(buffer[:count])
		remaining -= count
	}
	return nil
}
