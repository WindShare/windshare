//go:build windows

package outputwindows

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func newWindowsOutputObservedFile(native *windowsFile, private bool) *windowsOutputObservedFile {
	return &windowsOutputObservedFile{state: &windowsOutputFileState{native: native, private: private}}
}

func newWindowsOutputMutableFile(native *windowsFile, private bool) *windowsOutputMutableFile {
	return &windowsOutputMutableFile{state: &windowsOutputFileState{native: native, private: private}}
}

func windowsOutputFileStateFrom(file outputcap.FileIdentity) (*windowsOutputFileState, bool) {
	switch file := file.(type) {
	case *windowsOutputObservedFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *windowsOutputRecoveryDurabilityFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *windowsOutputMutableFile:
		if file != nil {
			return file.state, file.state != nil
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

func (file *windowsOutputObservedFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, windowsOutputError(err)
}

func (file *windowsOutputObservedFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputFileClose(file.state)
}

func (file *windowsOutputObservedFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputFileClosedError()
	}
	return windowsOutputFileSize(file.state)
}

func (file *windowsOutputObservedFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, windowsOutputFileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, windowsOutputError(err)
}

func (file *windowsOutputObservedFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputFileClosedError()
	}
	return windowsOutputSameFile(file.state, other)
}

func (file *windowsOutputRecoveryDurabilityFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputFileClose(file.state)
}

func (file *windowsOutputRecoveryDurabilityFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputFileClosedError()
	}
	return windowsOutputFileSize(file.state)
}

func (file *windowsOutputRecoveryDurabilityFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputFileClosedError()
	}
	return windowsOutputSameFile(file.state, other)
}

func (file *windowsOutputRecoveryDurabilityFile) Sync() error {
	if file == nil {
		return windowsOutputFileClosedError()
	}
	return windowsOutputFileSync(file.state)
}

func (file *windowsOutputMutableFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, windowsOutputError(err)
}

func (file *windowsOutputMutableFile) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputFileClosedError()
	}
	count, err := file.state.native.WriteAt(source, offset)
	return count, windowsOutputError(err)
}

func (file *windowsOutputMutableFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputFileClose(file.state)
}

func (file *windowsOutputMutableFile) Sync() error {
	if file == nil {
		return windowsOutputFileClosedError()
	}
	return windowsOutputFileSync(file.state)
}

func (file *windowsOutputMutableFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputFileClosedError()
	}
	return windowsOutputFileSize(file.state)
}

func (file *windowsOutputMutableFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.state == nil || file.state.native == nil {
		return windowsOutputFileClosedError()
	}
	return windowsOutputError(file.state.native.setModifiedTime(modified))
}

func (file *windowsOutputMutableFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, windowsOutputFileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, windowsOutputError(err)
}

func (file *windowsOutputMutableFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputFileClosedError()
	}
	return windowsOutputSameFile(file.state, other)
}

func newWindowsOutputLock(lock *windowsStableLock) *windowsOutputLock {
	state := &windowsOutputFileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &windowsOutputLock{
		native: lock,
		file:   &windowsOutputMutableFile{state: state},
	}
}

func (lock *windowsOutputLock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.state == nil ||
		lock.file.state.native == nil {
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
	if lock.file != nil && lock.file.state != nil {
		lock.file.state.native = nil
	}
	return windowsOutputError(err)
}
