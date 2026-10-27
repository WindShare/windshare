//go:build windows

package outputwindows

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func newWindowsOutputObservedFile(native *windowsFile, private bool) *windowsOutputObservedFile {
	return &windowsOutputObservedFile{State: &windowsOutputFileState{native: native, private: private}}
}

func newWindowsOutputMutableFile(native *windowsFile, private bool) *windowsOutputMutableFile {
	return &windowsOutputMutableFile{State: &windowsOutputFileState{native: native, private: private}}
}

func windowsOutputFileStateFrom(file outputcap.FileIdentity) (*windowsOutputFileState, bool) {
	switch file := file.(type) {
	case *windowsOutputObservedFile:
		if file != nil {
			return file.State, file.State != nil
		}
	case *windowsOutputRecoveryDurabilityFile:
		if file != nil {
			return file.State, file.State != nil
		}
	case *windowsOutputMutableFile:
		if file != nil {
			return file.State, file.State != nil
		}
	}
	return nil, false
}

func windowsOutputFileClosedError() error {
	return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
}

func windowsOutputFileClose(state *windowsOutputFileState) error {
	if state == nil || state.borrowed || state.native == nil {
		return nil
	}
	err := state.native.Close()
	state.native = nil
	return windowsOutputError(err)
}

func windowsOutputFileSize(state *windowsOutputFileState) (uint64, error) {
	if state == nil || state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	size, err := state.native.Size()
	return size, windowsOutputError(err)
}

func windowsOutputSameFile(
	state *windowsOutputFileState,
	other outputcap.FileIdentity,
) (bool, error) {
	right, ok := windowsOutputFileStateFrom(other)
	if !ok || state == nil || state.native == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file authority"))
	}
	same, err := sameWindowsOpenedObject(state.native, right.native)
	return same, windowsOutputError(err)
}

func windowsOutputFileSync(state *windowsOutputFileState) error {
	if state == nil || state.native == nil {
		return windowsOutputFileClosedError()
	}
	return windowsOutputError(state.native.Sync())
}

func (state *windowsOutputFileState) Close() error          { return windowsOutputFileClose(state) }
func (state *windowsOutputFileState) Size() (uint64, error) { return windowsOutputFileSize(state) }
func (state *windowsOutputFileState) SameFile(other outputcap.FileIdentity) (bool, error) {
	return windowsOutputSameFile(state, other)
}
func (state *windowsOutputFileState) Sync() error { return windowsOutputFileSync(state) }
func (state *windowsOutputFileState) ReadAt(data []byte, offset int64) (int, error) {
	if state == nil || state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	count, err := state.native.ReadAt(data, offset)
	return count, windowsOutputError(err)
}
func (state *windowsOutputFileState) WriteAt(data []byte, offset int64) (int, error) {
	if state == nil || state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	count, err := state.native.WriteAt(data, offset)
	return count, windowsOutputError(err)
}
func (state *windowsOutputFileState) SetModifiedTime(modified catalog.ModifiedTime) error {
	if state == nil || state.native == nil {
		return windowsOutputFileClosedError()
	}
	return windowsOutputError(state.native.setModifiedTime(modified))
}
func (state *windowsOutputFileState) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if state == nil || state.native == nil {
		return false, windowsOutputFileClosedError()
	}
	matches, err := state.native.metadataMatches(size, modified)
	return matches, windowsOutputError(err)
}

func newWindowsOutputLock(lock *windowsStableLock) *windowsOutputLock {
	state := &windowsOutputFileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &windowsOutputLock{
		native: lock,
		file:   &windowsOutputMutableFile{State: state},
	}
}

func (lock *windowsOutputLock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.State == nil ||
		lock.file.State.native == nil {
		return nil
	}
	return lock.file
}

func (lock *windowsOutputLock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil && lock.file.State != nil {
		lock.file.State.native = nil
	}
	return windowsOutputError(err)
}
