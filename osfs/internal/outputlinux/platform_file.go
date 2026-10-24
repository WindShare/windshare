//go:build linux

package outputlinux

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func (entry *linuxEntryRef) Kind() outputcap.EntryKind {
	if entry == nil || entry.native == nil {
		return outputcap.EntryAbsent
	}
	return entry.native.kind
}

func (entry *linuxEntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return linuxError(err)
}

func linuxFileStateFrom(file outputcap.FileIdentity) (*linuxFileState, bool) {
	switch file := file.(type) {
	case *linuxObservedFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *linuxRecoveryDurabilityFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *linuxMutableFile:
		if file != nil {
			return file.state, file.state != nil
		}
	}
	return nil, false
}

func linuxFileClosedError() error {
	return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux file authority is closed"))
}

func linuxFileClose(state *linuxFileState) error {
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
	return linuxError(errors.Join(originErr, nativeErr))
}

func linuxFileSize(state *linuxFileState) (uint64, error) {
	if state == nil || state.native == nil {
		return 0, linuxFileClosedError()
	}
	size, err := state.native.Size()
	return size, linuxError(err)
}

func linuxSameFile(state *linuxFileState, other outputcap.FileIdentity) (bool, error) {
	right, ok := linuxFileStateFrom(other)
	if !ok || state == nil || state.native == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file authority"))
	}
	same, err := linuxSameOpenRegularFile(state.native, right.native)
	return same, linuxError(err)
}

func linuxFileSync(state *linuxFileState) error {
	if state == nil || state.native == nil {
		return linuxFileClosedError()
	}
	return linuxError(state.native.sync())
}

func (file *linuxObservedFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxFileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, linuxError(err)
}

func (file *linuxObservedFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxFileClose(file.state)
}

func (file *linuxObservedFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxFileClosedError()
	}
	return linuxFileSize(file.state)
}

func (file *linuxObservedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, linuxFileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, linuxError(err)
}

func (file *linuxObservedFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxFileClosedError()
	}
	return linuxSameFile(file.state, other)
}

func (file *linuxRecoveryDurabilityFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxFileClose(file.state)
}

func (file *linuxRecoveryDurabilityFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxFileClosedError()
	}
	return linuxFileSize(file.state)
}

func (file *linuxRecoveryDurabilityFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxFileClosedError()
	}
	return linuxSameFile(file.state, other)
}

func (file *linuxRecoveryDurabilityFile) Sync() error {
	if file == nil {
		return linuxFileClosedError()
	}
	return linuxFileSync(file.state)
}

func (file *linuxMutableFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxFileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, linuxError(err)
}

func (file *linuxMutableFile) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, linuxFileClosedError()
	}
	count, err := file.state.native.WriteAt(source, offset)
	return count, linuxError(err)
}

func (file *linuxMutableFile) Close() error {
	if file == nil {
		return nil
	}
	return linuxFileClose(file.state)
}

func (file *linuxMutableFile) Sync() error {
	if file == nil {
		return linuxFileClosedError()
	}
	return linuxFileSync(file.state)
}

func (file *linuxMutableFile) Size() (uint64, error) {
	if file == nil {
		return 0, linuxFileClosedError()
	}
	return linuxFileSize(file.state)
}

func (file *linuxMutableFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.state == nil || file.state.native == nil {
		return linuxFileClosedError()
	}
	return linuxError(file.state.native.setModifiedTime(modified))
}

func (file *linuxMutableFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, linuxFileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, linuxError(err)
}

func (file *linuxMutableFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, linuxFileClosedError()
	}
	return linuxSameFile(file.state, other)
}

func newLinuxLock(lock *linuxOutputStableLock) *linuxLock {
	state := &linuxFileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &linuxLock{native: lock, file: &linuxMutableFile{state: state}}
}

func (lock *linuxLock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.state == nil ||
		lock.file.state.native == nil {
		return nil
	}
	return lock.file
}

func (lock *linuxLock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil && lock.file.state != nil {
		lock.file.state.native = nil
	}
	return linuxError(err)
}
