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

func (entry *linuxV3EntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return linuxV3Error(err)
}

func linuxV3FileStateFrom(file outputcap.FileIdentity) (*linuxV3FileState, bool) {
	switch file := file.(type) {
	case *linuxV3ObservedFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *linuxV3RecoveryDurabilityFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *linuxV3MutableFile:
		if file != nil {
			return file.state, file.state != nil
		}
	}
	return nil, false
}

func linuxV3FileClosedError() error {
	return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
}

func linuxV3FileClose(state *linuxV3FileState) error {
	if state == nil || state.borrowed {
		return nil
	}
	var originErr error
	if state.origin != nil && state.origin.parent != nil {
		originErr = state.origin.parent.close()
	}
	state.origin = nil
	var nativeErr error
	if state.native != nil {
		nativeErr = state.native.close()
	}
	state.native = nil
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func linuxV3FileSize(state *linuxV3FileState) (uint64, error) {
	if state == nil || state.native == nil {
		return 0, linuxV3FileClosedError()
	}
	size, err := state.native.Size()
	return size, linuxV3Error(err)
}

func linuxV3SameFile(state *linuxV3FileState, other outputcap.FileIdentity) (bool, error) {
	right, ok := linuxV3FileStateFrom(other)
	if !ok || state == nil || state.native == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file authority"))
	}
	same, err := linuxSameOpenRegularFile(state.native, right.native)
	return same, linuxV3Error(err)
}

func linuxV3FileSync(state *linuxV3FileState) error {
	if state == nil || state.native == nil {
		return linuxV3FileClosedError()
	}
	return linuxV3Error(state.native.sync())
}

func (file *linuxV3ObservedFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxV3FileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3ObservedFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxV3FileClose(file.state)
}

func (file *linuxV3ObservedFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxV3FileClosedError()
	}
	return linuxV3FileSize(file.state)
}

func (file *linuxV3ObservedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, linuxV3FileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, linuxV3Error(err)
}

func (file *linuxV3ObservedFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxV3FileClosedError()
	}
	return linuxV3SameFile(file.state, other)
}

func (file *linuxV3RecoveryDurabilityFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxV3FileClose(file.state)
}

func (file *linuxV3RecoveryDurabilityFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxV3FileClosedError()
	}
	return linuxV3FileSize(file.state)
}

func (file *linuxV3RecoveryDurabilityFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxV3FileClosedError()
	}
	return linuxV3SameFile(file.state, other)
}

func (file *linuxV3RecoveryDurabilityFile) Sync() error {
	if file == nil {
		return linuxV3FileClosedError()
	}
	return linuxV3FileSync(file.state)
}

func (file *linuxV3MutableFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxV3FileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3MutableFile) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxV3FileClosedError()
	}
	count, err := file.state.native.WriteAt(source, offset)
	return count, linuxV3Error(err)
}

func (file *linuxV3MutableFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxV3FileClose(file.state)
}

func (file *linuxV3MutableFile) Sync() error {
	if file == nil {
		return linuxV3FileClosedError()
	}
	return linuxV3FileSync(file.state)
}

func (file *linuxV3MutableFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxV3FileClosedError()
	}
	return linuxV3FileSize(file.state)
}

func (file *linuxV3MutableFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.state == nil || file.state.native == nil {
		return linuxV3FileClosedError()
	}
	return linuxV3Error(file.state.native.setModifiedTime(modified))
}

func (file *linuxV3MutableFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, linuxV3FileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, linuxV3Error(err)
}

func (file *linuxV3MutableFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxV3FileClosedError()
	}
	return linuxV3SameFile(file.state, other)
}

func newLinuxV3Lock(lock *linuxOutputStableLock) *linuxV3Lock {
	state := &linuxV3FileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &linuxV3Lock{native: lock, file: &linuxV3MutableFile{state: state}}
}

func (lock *linuxV3Lock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.state == nil ||
		lock.file.state.native == nil {
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
	if lock.file != nil && lock.file.state != nil {
		lock.file.state.native = nil
	}
	return linuxV3Error(err)
}
