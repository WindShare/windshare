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
			return file.State, file.State != nil
		}
	case *linuxRecoveryDurabilityFile:
		if file != nil {
			return file.State, file.State != nil
		}
	case *linuxMutableFile:
		if file != nil {
			return file.State, file.State != nil
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

func (state *linuxFileState) Close() error          { return linuxFileClose(state) }
func (state *linuxFileState) Size() (uint64, error) { return linuxFileSize(state) }
func (state *linuxFileState) SameFile(other outputcap.FileIdentity) (bool, error) {
	return linuxSameFile(state, other)
}
func (state *linuxFileState) Sync() error { return linuxFileSync(state) }
func (state *linuxFileState) ReadAt(data []byte, offset int64) (int, error) {
	if state == nil || state.native == nil {
		return 0, linuxFileClosedError()
	}
	count, err := state.native.ReadAt(data, offset)
	return count, linuxError(err)
}
func (state *linuxFileState) WriteAt(data []byte, offset int64) (int, error) {
	if state == nil || state.native == nil {
		return 0, linuxFileClosedError()
	}
	count, err := state.native.WriteAt(data, offset)
	return count, linuxError(err)
}
func (state *linuxFileState) SetModifiedTime(modified catalog.ModifiedTime) error {
	if state == nil || state.native == nil {
		return linuxFileClosedError()
	}
	return linuxError(state.native.setModifiedTime(modified))
}
func (state *linuxFileState) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if state == nil || state.native == nil {
		return false, linuxFileClosedError()
	}
	matches, err := state.native.metadataMatches(size, modified)
	return matches, linuxError(err)
}

func newLinuxLock(lock *linuxOutputStableLock) *linuxLock {
	state := &linuxFileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &linuxLock{native: lock, file: &linuxMutableFile{State: state}}
}

func (lock *linuxLock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.State == nil ||
		lock.file.State.native == nil {
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
	if lock.file != nil && lock.file.State != nil {
		lock.file.State.native = nil
	}
	return linuxError(err)
}
