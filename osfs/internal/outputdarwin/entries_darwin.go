//go:build darwin

package outputdarwin

import (
	"errors"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

type entryReference struct {
	file   *os.File
	object objectIdentity
	name   string
}

func (entry *entryReference) Kind() outputcap.EntryKind {
	if entry == nil || entry.file == nil {
		return outputcap.EntryAbsent
	}
	return entryKind(entry.object.kind)
}
func (entry *entryReference) Close() error {
	if entry == nil || entry.file == nil {
		return nil
	}
	err := entry.file.Close()
	entry.file = nil
	return err
}
func (dir *directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if err := errors.Join(dir.verify(), validComponent(name)); err != nil {
		return nil, err
	}
	var before unix.Stat_t
	if err := unix.Fstatat(dir.fd(), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, nativeError(err)
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if before.Mode&unix.S_IFMT == unix.S_IFLNK {
		flags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_SYMLINK
	}
	fd, err := unix.Openat(dir.fd(), name, flags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, errors.Join(nativeError(err), unix.Close(fd))
	}
	if identity(before) != identity(after) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, unix.Close(fd))
	}
	return &entryReference{file: os.NewFile(uintptr(fd), name), object: identity(after), name: name}, nil
}
func (dir *directory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	ref, ok := expected.(*entryReference)
	if !ok || ref == nil || ref.file == nil {
		return false, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.verify(), validComponent(name)); err != nil {
		return false, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dir.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, nativeError(err)
	}
	return identity(stat) == ref.object, nil
}
func (dir *directory) OpenPinnedDirectory(expected outputcap.CurrentEntryReference, private bool) (outputcap.Directory, error) {
	ref, ok := expected.(*entryReference)
	if !ok || ref.Kind() != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	match, err := dir.EntryMatches(ref.name, ref)
	if err != nil || !match {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	opened, err := dir.OpenDirectory(ref.name, private)
	if err != nil {
		return nil, err
	}
	if opened.(*directory).object != ref.object {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, opened.Close())
	}
	return opened, nil
}
func (dir *directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	if err := dir.ValidateCreateAuthority(); err != nil {
		return err
	}
	match, err := dir.EntryMatches(name, expected)
	if err != nil {
		return err
	}
	if !match {
		kind, err := dir.ObserveEntry(name)
		if err == nil && kind == outputcap.EntryAbsent {
			return nil
		}
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	flags := 0
	if expected.Kind() == outputcap.EntryDirectory {
		flags = unix.AT_REMOVEDIR
	}
	return nativeError(unix.Unlinkat(dir.fd(), name, flags))
}
func (dir *directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	right, ok := expected.(*directory)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), right.verify(), validComponent(name)); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dir.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return nativeError(err)
	}
	if identity(stat) != right.object {
		return outputcap.ErrUnsafeNamespace
	}
	return nativeError(unix.Unlinkat(dir.fd(), name, unix.AT_REMOVEDIR))
}
func (dir *directory) InstallDirectoryNoReplace(candidate outputcap.Directory, name string) (outputcap.Directory, error) {
	source, ok := candidate.(*directory)
	if !ok || source.parent == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), source.verify(), validComponent(name)); err != nil {
		return nil, err
	}
	current, err := source.parent.OpenDirectory(source.name, source.private)
	if err != nil {
		return nil, err
	}
	same, compareErr := current.SameDirectory(source)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, compareErr, closeErr)
	}
	if err := unix.RenameatxNp(source.parent.fd(), source.name, dir.fd(), name, unix.RENAME_EXCL|unix.RENAME_NOFOLLOW_ANY); err != nil {
		return nil, nativeError(err)
	}
	opened, err := dir.OpenDirectory(name, source.private)
	if err != nil {
		return nil, err
	}
	same, err = opened.SameDirectory(source)
	if err != nil || !same {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, opened.Close())
	}
	return opened, nil
}
