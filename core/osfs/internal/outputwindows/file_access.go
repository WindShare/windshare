//go:build windows

package outputwindows

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func newWindowsOutputV3ObservedFile(native *windowsV3File, private bool) *windowsOutputV3ObservedFile {
	return &windowsOutputV3ObservedFile{state: &windowsOutputV3FileState{native: native, private: private}}
}

func newWindowsOutputV3MutableFile(native *windowsV3File, private bool) *windowsOutputV3MutableFile {
	return &windowsOutputV3MutableFile{state: &windowsOutputV3FileState{native: native, private: private}}
}

func windowsOutputV3FileStateFrom(file outputcap.FileIdentity) (*windowsOutputV3FileState, bool) {
	switch file := file.(type) {
	case *windowsOutputV3ObservedFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *windowsOutputV3RecoveryDurabilityFile:
		if file != nil {
			return file.state, file.state != nil
		}
	case *windowsOutputV3MutableFile:
		if file != nil {
			return file.state, file.state != nil
		}
	}
	return nil, false
}

func windowsOutputV3FileClosedError() error {
	return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
}

func windowsOutputV3FileClose(state *windowsOutputV3FileState) error {
	if state == nil || state.borrowed || state.native == nil {
		return nil
	}
	err := state.native.Close()
	state.native = nil
	return windowsOutputV3Error(err)
}

func windowsOutputV3FileSize(state *windowsOutputV3FileState) (uint64, error) {
	if state == nil || state.native == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	size, err := state.native.Size()
	return size, windowsOutputV3Error(err)
}

func windowsOutputV3SameFile(
	state *windowsOutputV3FileState,
	other outputcap.FileIdentity,
) (bool, error) {
	right, ok := windowsOutputV3FileStateFrom(other)
	if !ok || state == nil || state.native == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file authority"))
	}
	same, err := sameWindowsV3OpenedObject(state.native, right.native)
	return same, windowsOutputV3Error(err)
}

func windowsOutputV3FileSync(state *windowsOutputV3FileState) error {
	if state == nil || state.native == nil {
		return windowsOutputV3FileClosedError()
	}
	return windowsOutputV3Error(state.native.Sync())
}

func (file *windowsOutputV3ObservedFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3ObservedFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputV3FileClose(file.state)
}

func (file *windowsOutputV3ObservedFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3FileSize(file.state)
}

func (file *windowsOutputV3ObservedFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, windowsOutputV3FileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, windowsOutputV3Error(err)
}

func (file *windowsOutputV3ObservedFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3SameFile(file.state, other)
}

func (file *windowsOutputV3RecoveryDurabilityFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputV3FileClose(file.state)
}

func (file *windowsOutputV3RecoveryDurabilityFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3FileSize(file.state)
}

func (file *windowsOutputV3RecoveryDurabilityFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3SameFile(file.state, other)
}

func (file *windowsOutputV3RecoveryDurabilityFile) Sync() error {
	if file == nil {
		return windowsOutputV3FileClosedError()
	}
	return windowsOutputV3FileSync(file.state)
}

func (file *windowsOutputV3MutableFile) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	count, err := file.state.native.ReadAt(destination, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3MutableFile) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	count, err := file.state.native.WriteAt(source, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3MutableFile) Close() error {
	if file == nil {
		return nil
	}
	return windowsOutputV3FileClose(file.state)
}

func (file *windowsOutputV3MutableFile) Sync() error {
	if file == nil {
		return windowsOutputV3FileClosedError()
	}
	return windowsOutputV3FileSync(file.state)
}

func (file *windowsOutputV3MutableFile) Size() (uint64, error) {
	if file == nil {
		return 0, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3FileSize(file.state)
}

func (file *windowsOutputV3MutableFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.state == nil || file.state.native == nil {
		return windowsOutputV3FileClosedError()
	}
	return windowsOutputV3Error(file.state.native.setModifiedTime(modified))
}

func (file *windowsOutputV3MutableFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	if file == nil || file.state == nil || file.state.native == nil {
		return false, windowsOutputV3FileClosedError()
	}
	matches, err := file.state.native.metadataMatches(size, modified)
	return matches, windowsOutputV3Error(err)
}

func (file *windowsOutputV3MutableFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file == nil {
		return false, windowsOutputV3FileClosedError()
	}
	return windowsOutputV3SameFile(file.state, other)
}

func newWindowsOutputV3Lock(lock *windowsV3StableLock) *windowsOutputV3Lock {
	state := &windowsOutputV3FileState{private: true, borrowed: true}
	if lock != nil {
		state.native = lock.file
	}
	return &windowsOutputV3Lock{
		native: lock,
		file:   &windowsOutputV3MutableFile{state: state},
	}
}

func (lock *windowsOutputV3Lock) File() outputcap.MutableFile {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.state == nil ||
		lock.file.state.native == nil {
		return nil
	}
	return lock.file
}

func (lock *windowsOutputV3Lock) Close() error {
	if lock == nil || lock.native == nil {
		return nil
	}
	err := lock.native.Close()
	lock.native = nil
	if lock.file != nil && lock.file.state != nil {
		lock.file.state.native = nil
	}
	return windowsOutputV3Error(err)
}
