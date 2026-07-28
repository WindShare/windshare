//go:build linux

package outputlinux

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (entry *linuxV3EntryRef) Kind() outputcap.EntryKind {
	if entry == nil || entry.native == nil {
		return outputcap.EntryAbsent
	}
	return entry.native.kind
}

func (entry *linuxV3EntryRef) AllocatedSize() (uint64, error) {
	if entry == nil || entry.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux pinned entry is closed"))
	}
	size, err := entry.native.allocatedSize()
	return size, linuxV3Error(err)
}

func (entry *linuxV3EntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return linuxV3Error(err)
}

func newLinuxV3Lock(lock *linuxOutputStableLock) *linuxV3Lock {
	file := &linuxV3File{private: true, borrowed: true}
	if lock != nil {
		file.native = lock.file
	}
	return &linuxV3Lock{native: lock, file: file}
}

func (file *linuxV3File) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	count, err := file.native.ReadAt(destination, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3File) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	count, err := file.native.WriteAt(source, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3File) Close() error {
	if file == nil {
		return nil
	}
	if file.borrowed {
		return nil
	}
	var originErr error
	if file.origin != nil && file.origin.parent != nil {
		originErr = file.origin.parent.close()
	}
	file.origin = nil
	var nativeErr error
	if file.native != nil {
		nativeErr = file.native.close()
	}
	file.native = nil
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func (file *linuxV3File) Sync() error {
	if file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.sync())
}

func (file *linuxV3File) Truncate(size int64) error {
	if file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.truncate(size))
}

func (file *linuxV3File) Size() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	size, err := file.native.Size()
	return size, linuxV3Error(err)
}

func (file *linuxV3File) AllocatedSize() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	size, err := file.native.allocatedSize()
	return size, linuxV3Error(err)
}

func (file *linuxV3File) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	return linuxV3Error(file.native.setModifiedTime(modified))
}

func (file *linuxV3File) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
	}
	matches, err := file.native.metadataMatches(size, modified)
	return matches, linuxV3Error(err)
}

func (file *linuxV3File) SameFile(other outputcap.File) (bool, error) {
	right, ok := other.(*linuxV3File)
	if !ok || file == nil || file.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file authority"))
	}
	same, err := linuxSameOpenRegularFile(file.native, right.native)
	return same, linuxV3Error(err)
}

func (lock *linuxV3Lock) File() outputcap.File {
	if lock == nil {
		return nil
	}
	return lock.file
}

func (lock *linuxV3Lock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil {
		lock.file.native = nil
	}
	return linuxV3Error(err)
}
