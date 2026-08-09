//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func (directory *windowsOutputV3Directory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.identityClaim()
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	if directory == nil || directory.native == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	claim, err := directory.native.prepareIdentityClaim()
	return claim, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) Close() error {
	if directory == nil || directory.native == nil {
		return nil
	}
	err := directory.native.Close()
	directory.native = nil
	return windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.Sync())
}

func (directory *windowsOutputV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) ValidatePublicEntryNames(names []string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.validatePublicEntryNames(names))
}

func (directory *windowsOutputV3Directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Windows directory authority is closed"),
		)
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3EntryRef{native: pinned}, nil
}

func (directory *windowsOutputV3Directory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: opened}, nil
}

func (directory *windowsOutputV3Directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*windowsOutputV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows pinned entry removal"))
	}
	return windowsOutputV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *windowsOutputV3Directory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory authority"))
	}
	same, err := sameWindowsV3OpenedDirectory(directory.native, right.native)
	return same, windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	return windowsOutputV3Error(directory.native.setModifiedTime(modified))
}

func (directory *windowsOutputV3Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsV3Directory
	var err error
	if private {
		native, err = directory.native.OpenPrivateDirectory(name)
	} else {
		native, err = directory.native.OpenDirectory(name)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	var native *windowsV3Directory
	var err error
	if private {
		native, err = directory.native.CreatePrivateDirectory(name)
	} else {
		native, err = directory.native.openDirectory(name, false, windows.FILE_CREATE)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: native}, nil
}

func (directory *windowsOutputV3Directory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory installation authority"))
	}
	installed, err := directory.native.InstallPrivateDirectoryNoReplace(source.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3Directory{native: installed}, nil
}

func (directory *windowsOutputV3Directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*windowsOutputV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows directory removal authority"))
	}
	return windowsOutputV3Error(directory.native.RemoveDirectory(name, target.native))
}

func (directory *windowsOutputV3Directory) CreateFile(name string, private bool, size int64) (outputcap.File, error) {
	if directory == nil || directory.native == nil || size < 0 {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: invalid Windows file creation authority or size"))
	}
	var native *windowsV3File
	var err error
	if private {
		native, err = directory.native.CreatePrivateFile(name)
	} else {
		native, _, err = directory.native.openFile(
			name, windows.FILE_CREATE, windowsV3PrivateFileAccess(), nil, false,
		)
	}
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	if err := native.Truncate(size); err != nil {
		removeErr := directory.native.RemoveRegularLink(name, native)
		return nil, windowsOutputV3Error(errors.Join(err, removeErr, native.Close()))
	}
	actual, err := native.Size()
	if err != nil || actual != uint64(size) {
		removeErr := directory.native.RemoveRegularLink(name, native)
		return nil, windowsOutputV3Error(errors.Join(
			errors.New("osfs: Windows file creation did not install the exact size"), err, removeErr, native.Close(),
		))
	}
	return &windowsOutputV3File{native: native, private: private}, nil
}

func (directory *windowsOutputV3Directory) OpenFile(name string, private, writable bool) (outputcap.File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	access := windowsV3ReadFileAccess()
	if writable {
		access = windowsV3PrivateFileAccess()
	}
	native, _, err := directory.native.openFile(name, windows.FILE_OPEN, access, nil, private)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3File{native: native, private: private}, nil
}

func (directory *windowsOutputV3Directory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	file, ok := source.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file link authority"))
	}
	linked, err := directory.native.LinkRegularFileNoReplace(file.native, name)
	if err != nil {
		return nil, windowsOutputV3Error(err)
	}
	return &windowsOutputV3File{native: linked}, nil
}

func (directory *windowsOutputV3Directory) ReplacePrivateFile(source outputcap.File, name string) error {
	file, ok := source.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil || !file.private {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows private state replacement authority"))
	}
	return windowsOutputV3Error(directory.native.AtomicReplacePrivateFile(file.native, name))
}

func (directory *windowsOutputV3Directory) RemoveFile(name string, expected outputcap.File) error {
	file, ok := expected.(*windowsOutputV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Windows file removal authority"))
	}
	return windowsOutputV3Error(directory.native.RemoveRegularLink(name, file.native))
}

func (entry *windowsOutputV3EntryRef) Kind() outputcap.EntryKind {
	if entry == nil || entry.native == nil {
		return outputcap.EntryAbsent
	}
	return entry.native.kind
}

func (entry *windowsOutputV3EntryRef) Close() error {
	if entry == nil || entry.native == nil {
		return nil
	}
	err := entry.native.close()
	entry.native = nil
	return windowsOutputV3Error(err)
}

func (directory *windowsOutputV3Directory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.AcquireExistingStableLock(name)
		if err != nil {
			return nil, false, windowsOutputV3Error(err)
		}
		return newWindowsOutputV3Lock(lock), false, nil
	}
	lock, created, err := directory.native.AcquireStableLock(name)
	if err != nil {
		return nil, false, windowsOutputV3Error(err)
	}
	return newWindowsOutputV3Lock(lock), created, nil
}

func newWindowsOutputV3Lock(lock *windowsV3StableLock) *windowsOutputV3Lock {
	file := &windowsOutputV3File{private: true, borrowed: true}
	if lock != nil {
		file.native = lock.file
	}
	return &windowsOutputV3Lock{native: lock, file: file}
}

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

func (file *windowsOutputV3File) Size() (uint64, error) {
	if file == nil || file.native == nil {
		return 0, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Windows file authority is closed"))
	}
	size, err := file.native.Size()
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
	_ outputcap.Platform                            = (*windowsOutputV3Platform)(nil)
	_ outputcap.Directory                           = (*windowsOutputV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentity         = (*windowsOutputV3Directory)(nil)
	_ outputcap.PersistentDirectoryIdentityPreparer = (*windowsOutputV3Directory)(nil)
	_ outputcap.File                                = (*windowsOutputV3File)(nil)
	_ outputcap.Lock                                = (*windowsOutputV3Lock)(nil)
)
