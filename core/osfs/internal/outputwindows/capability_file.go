//go:build windows

package outputwindows

import (
	"encoding/binary"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const windowsCloseRevalidationIdentityDomain = "windows/ntfs/transient-file-id/v1"

func (file *windowsOutputV3File) ReadAt(destination []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	count, err := file.native.ReadAt(destination, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) WriteAt(source []byte, offset int64) (int, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	count, err := file.native.WriteAt(source, offset)
	return count, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) Close() error {
	if file == nil || file.borrowed || file.native == nil {
		return nil
	}
	err := file.native.Close()
	file.native = nil
	return windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) Sync() error {
	if file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	return windowsOutputV3Error(file.native.Sync())
}

func (file *windowsOutputV3File) Truncate(size int64) error {
	if file == nil || file.native == nil || size < 0 {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: invalid Windows file authority or size"))
	}
	if err := file.native.Truncate(size); err != nil {
		return windowsOutputV3Error(err)
	}
	actual, err := file.native.Size()
	if err != nil {
		return windowsOutputV3Error(err)
	}
	if actual != uint64(size) {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file size differs after truncate"))
	}
	return nil
}

func (file *windowsOutputV3File) Size() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	size, err := file.native.Size()
	return size, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) AllocatedSize() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	size, err := file.native.allocatedSize()
	return size, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	return windowsOutputV3Error(file.native.setModifiedTime(modified))
}

func (file *windowsOutputV3File) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	matches, err := file.native.metadataMatches(size, modified)
	return matches, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) SameFile(other outputcap.File) (bool, error) {
	right, ok := other.(*windowsOutputV3File)
	if !ok || file == nil || file.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file authority"))
	}
	same, err := sameWindowsV3OpenedObject(file.native, right.native)
	return same, windowsOutputV3Error(err)
}

func (file *windowsOutputV3File) CloseRevalidationIdentity() (outputcap.TransientFileIdentity, error) {
	if file == nil || file.native == nil || file.native.inspector == nil {
		return outputcap.TransientFileIdentity{}, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows file authority is closed"),
		)
	}
	if err := file.native.verify(file.private); err != nil {
		return outputcap.TransientFileIdentity{}, windowsOutputV3Error(err)
	}
	facts, err := file.native.inspector.Inspect(file.native.handle())
	if err != nil || !facts.object.valid() {
		return outputcap.TransientFileIdentity{}, windowsOutputV3Error(errors.Join(
			err,
			errors.New("osfs: Windows file identity is unavailable"),
		))
	}
	guid := []byte(facts.object.volume.guid)
	encoded := make([]byte, 8+len(facts.object.fileID)+len(guid))
	binary.LittleEndian.PutUint64(encoded[:8], facts.object.volume.serial)
	copy(encoded[8:24], facts.object.fileID[:])
	copy(encoded[24:], guid)
	return outputcap.NewTransientFileIdentity(windowsCloseRevalidationIdentityDomain, encoded), nil
}

func (lock *windowsOutputV3Lock) File() outputcap.File {
	if lock == nil || lock.native == nil || lock.file == nil || lock.file.native == nil {
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
	if lock.file != nil {
		lock.file.native = nil
	}
	return windowsOutputV3Error(err)
}

func windowsOutputV3Error(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errWindowsV3OutputUnsupported):
		return errors.Join(outputcap.ErrRecoverableOutputUnsupported, err)
	case errors.Is(err, errWindowsV3OutputUnsafe):
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	case errors.Is(err, errWindowsV3OutputCollision):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	case errors.Is(err, errWindowsV3OutputLockBusy):
		return errors.Join(outputcap.ErrNamespaceLockBusy, err)
	case errors.Is(err, fs.ErrExist):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	default:
		return err
	}
}

var (
	_ outputcap.Platform  = (*windowsOutputV3Platform)(nil)
	_ outputcap.Directory = (*windowsOutputV3Directory)(nil)
	_ outputcap.File      = (*windowsOutputV3File)(nil)
	_ outputcap.Lock      = (*windowsOutputV3Lock)(nil)
)
