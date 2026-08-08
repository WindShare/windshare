//go:build linux

package outputlinux

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (directory *linuxV3Directory) Close() error {
	if directory == nil {
		return nil
	}
	var originErr error
	if directory.origin != nil && directory.origin.parent != nil {
		originErr = directory.origin.parent.close()
	}
	directory.origin = nil
	var nativeErr error
	if directory.native != nil {
		nativeErr = directory.native.close()
	}
	directory.native = nil
	return linuxV3Error(errors.Join(originErr, nativeErr))
}

func (directory *linuxV3Directory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	native, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(err)
	}
	result := &linuxV3Directory{native: native}
	if directory.origin != nil {
		parent, duplicateErr := directory.origin.parent.Duplicate()
		if duplicateErr != nil {
			return nil, linuxV3Error(errors.Join(duplicateErr, native.close()))
		}
		result.origin = &linuxV3DirectoryOrigin{parent: parent, name: directory.origin.name}
	}
	return result, nil
}

func (directory *linuxV3Directory) Sync() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.sync())
}

func (directory *linuxV3Directory) Names(limit int) ([]string, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	names, err := directory.native.names(limit)
	return names, linuxV3Error(err)
}

func (directory *linuxV3Directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	kind, err := directory.native.observeEntry(name)
	return kind, linuxV3Error(err)
}

func (directory *linuxV3Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory == nil || directory.native == nil {
		return outputcap.EntryAbsent, false, errors.Join(
			outputcap.ErrUnsafeNamespace,
			errors.New("osfs: Linux directory authority is closed"),
		)
	}
	kind, exact, err := directory.native.classifyExactEntry(name)
	return kind, exact, linuxV3Error(err)
}

func (directory *linuxV3Directory) validatePublicEntryName(name string) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	_, exact, err := directory.native.classifyExactEntry(name)
	if err != nil {
		return linuxV3Error(err)
	}
	if !exact {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (directory *linuxV3Directory) ValidatePublicEntryNames(names []string) error {
	for _, name := range names {
		if err := directory.validatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *linuxV3Directory) ValidateCreateAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.validateCreateAuthority())
}

func (directory *linuxV3Directory) ValidateMetadataAuthority() error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if err := directory.native.validateMetadataAuthority(); err != nil {
		return linuxV3Error(err)
	}
	return nil
}

func (directory *linuxV3Directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	pinned, err := directory.native.openPinnedEntry(name)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3EntryRef{native: pinned}, nil
}

func (directory *linuxV3Directory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry"))
	}
	matches, err := directory.native.pinnedEntryMatches(name, pinned.native)
	return matches, linuxV3Error(err)
}

func (directory *linuxV3Directory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference, private bool,
) (outputcap.Directory, error) {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned directory"))
	}
	opened, err := directory.native.openPinnedDirectory(pinned.native, private)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return &linuxV3Directory{native: opened}, nil
}

func (directory *linuxV3Directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	pinned, ok := expected.(*linuxV3EntryRef)
	if !ok || directory == nil || directory.native == nil || pinned == nil || pinned.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux pinned entry removal"))
	}
	return linuxV3Error(directory.native.removePinnedEntry(name, pinned.native))
}

func (directory *linuxV3Directory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || right == nil || right.native == nil {
		return false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory authority"))
	}
	same, err := linuxSameOpenDirectory(directory.native, right.native)
	return same, linuxV3Error(err)
}

func (directory *linuxV3Directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if directory == nil || directory.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	return linuxV3Error(directory.native.setModifiedTime(modified))
}

func (directory *linuxV3Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputDirectory
	var err error
	if private {
		opened, err = directory.native.openDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		opened, err = directory.native.openDirectory(name)
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(opened, name)
}

func (directory *linuxV3Directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var created *linuxOutputDirectory
	var err error
	if private {
		created, err = directory.native.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
	} else {
		created, err = directory.native.createDirectoryExact(name, uint32(dirPerm))
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindDirectoryOrigin(created, name)
}

func (directory *linuxV3Directory) bindDirectoryOrigin(
	child *linuxOutputDirectory,
	name string,
) (outputcap.Directory, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, child.close()))
	}
	return &linuxV3Directory{
		native: child,
		origin: &linuxV3DirectoryOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	source, ok := candidate.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || source == nil || source.native == nil ||
		source.origin == nil || source.origin.parent == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux installation candidate has no fixed origin"))
	}
	if err := source.origin.parent.renameDirectory(
		source.origin.name, source.native, directory.native, name, linuxRenameNoReplace,
	); err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.OpenDirectory(name, true)
}

func (directory *linuxV3Directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	target, ok := expected.(*linuxV3Directory)
	if !ok || directory == nil || directory.native == nil || target == nil || target.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux directory removal authority"))
	}
	return linuxV3Error(directory.native.unlinkDirectory(name, target.native))
}

func (directory *linuxV3Directory) CreateFile(name string, private bool, size int64) (outputcap.File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	permissions := uint32(filePerm)
	if private {
		permissions = linuxOutputStateFileMode
	}
	created, err := directory.native.createRegularFileExact(name, permissions, size)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, created.close()))
	}
	return &linuxV3File{
		native: created, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) OpenFile(name string, private, writable bool) (outputcap.File, error) {
	if directory == nil || directory.native == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	var opened *linuxOutputRegularFile
	var err error
	if private {
		opened, err = directory.native.openRegularFileExact(name, writable, linuxOutputStateFileMode)
	} else {
		opened, err = directory.native.openRegularFile(name, writable)
	}
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindFileOrigin(opened, name, private)
}

func (directory *linuxV3Directory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	file, ok := source.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil ||
		file.origin == nil || file.origin.parent == nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file link authority"))
	}
	if err := directory.native.linkRegularFileNoReplace(
		file.origin.parent, file.origin.name, file.native, name,
	); err != nil {
		return nil, linuxV3Error(err)
	}
	linked, err := directory.native.openRegularFile(name, false)
	if err != nil {
		return nil, linuxV3Error(err)
	}
	return directory.bindFileOrigin(linked, name, false)
}

func (directory *linuxV3Directory) bindFileOrigin(
	file *linuxOutputRegularFile,
	name string,
	private bool,
) (outputcap.File, error) {
	parent, err := directory.native.Duplicate()
	if err != nil {
		return nil, linuxV3Error(errors.Join(err, file.close()))
	}
	return &linuxV3File{
		native: file, private: private,
		origin: &linuxV3FileOrigin{parent: parent, name: name},
	}, nil
}

func (directory *linuxV3Directory) ReplacePrivateFile(source outputcap.File, name string) error {
	file, ok := source.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil ||
		!file.private || file.origin == nil || file.origin.parent == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux state replacement source has no private fixed origin"))
	}
	return linuxV3Error(file.origin.parent.renameRegularFile(
		file.origin.name, file.native, directory.native, name, linuxRenameReplace,
	))
}

func (directory *linuxV3Directory) RemoveFile(name string, expected outputcap.File) error {
	file, ok := expected.(*linuxV3File)
	if !ok || directory == nil || directory.native == nil || file == nil || file.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: incompatible Linux file removal authority"))
	}
	return linuxV3Error(directory.native.unlinkRegularFile(name, file.native))
}

func (directory *linuxV3Directory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory == nil || directory.native == nil {
		return nil, false, errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux directory authority is closed"))
	}
	if existingOnly {
		lock, err := directory.native.acquireExistingStableLock(name)
		if err != nil {
			return nil, false, linuxV3Error(err)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	file, err := directory.native.createRegularFileExact(name, linuxOutputStateFileMode, 0)
	created := err == nil
	if errors.Is(err, errLinuxOutputCollision) {
		lock, existingErr := directory.native.acquireExistingStableLock(name)
		if existingErr != nil {
			return nil, false, linuxV3Error(existingErr)
		}
		return newLinuxV3Lock(lock), false, nil
	}
	if err != nil {
		return nil, false, linuxV3Error(err)
	}
	lock, err := linuxLockStableFile(file)
	if err != nil {
		return nil, false, linuxV3Error(errors.Join(err, file.close()))
	}
	matches, err := directory.native.regularEntryMatches(name, file)
	if err != nil || !matches {
		return nil, false, linuxV3Error(errors.Join(
			linuxUnsafe("lock stable output authority", "lock name differs from the locked object", nil),
			err,
			lock.Close(),
		))
	}
	return newLinuxV3Lock(lock), created, nil
}
