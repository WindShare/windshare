//go:build linux

package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"math"

	"golang.org/x/sys/unix"
)

type linuxOutputPinnedEntry struct {
	system      *linuxOutputSystem
	fd          int
	certificate linuxOutputCertificate
	object      linuxOpenObjectIdentity
	kind        outputV3EntryKind
	name        string
}

func (directory *linuxOutputDirectory) openPinnedEntry(
	name string,
) (_ *linuxOutputPinnedEntry, resultErr error) {
	const operation = "pin output entry"
	if err := directory.verifyHandle(); err != nil {
		return nil, err
	}
	if err := linuxValidateComponent(operation, name); err != nil {
		return nil, err
	}
	before, err := directory.namedIdentityNoFollow(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	flags := unix.O_PATH
	switch linuxFileType(before.mode) {
	case unix.S_IFREG:
		flags = unix.O_RDONLY
	case unix.S_IFDIR:
		flags = unix.O_RDONLY | unix.O_DIRECTORY
	}
	fd, err := directory.openRelative(name, flags, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, directory.system.close(fd))
		}
	}()
	opened, err := linuxVerifyOpenObject(directory.system, fd, directory.certificate)
	if err != nil {
		return nil, err
	}
	after, err := directory.namedIdentityNoFollow(name)
	if err != nil {
		return nil, err
	}
	if !linuxSameNamespaceObject(before, opened) || !linuxSameNamespaceObject(opened, after) {
		return nil, linuxUnsafe(operation, "entry changed while its no-follow handle was pinned", nil)
	}
	return &linuxOutputPinnedEntry{
		system: directory.system, fd: fd, certificate: directory.certificate,
		object: opened, kind: linuxOutputEntryKind(opened.mode), name: name,
	}, nil
}

func linuxSameNamespaceObject(left, right linuxOpenObjectIdentity) bool {
	return left.mountID == right.mountID &&
		left.deviceMajor == right.deviceMajor && left.deviceMinor == right.deviceMinor &&
		left.inode == right.inode && linuxFileType(left.mode) == linuxFileType(right.mode)
}

func (entry *linuxOutputPinnedEntry) close() error {
	if entry == nil || entry.fd < 0 {
		return nil
	}
	err := entry.system.close(entry.fd)
	entry.fd = -1
	return err
}

func (entry *linuxOutputPinnedEntry) allocatedSize() (uint64, error) {
	const operation = "inspect pinned output entry allocation"
	if entry == nil || entry.system == nil || entry.fd < 0 {
		return 0, linuxUnsafe(operation, "entry handle is closed or absent", nil)
	}
	current, err := linuxVerifyOpenObject(entry.system, entry.fd, entry.certificate)
	if err != nil {
		return 0, err
	}
	if !entry.object.sameObject(current) || linuxFileType(entry.object.mode) != linuxFileType(current.mode) {
		return 0, linuxUnsafe(operation, "entry handle changed after it was pinned", nil)
	}
	requested := unix.STATX_TYPE | unix.STATX_INO | unix.STATX_BLOCKS | unix.STATX_MNT_ID_UNIQUE
	var stat unix.Statx_t
	if err := entry.system.statx(
		entry.fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, requested, &stat,
	); err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	if stat.Mask&uint32(requested) != uint32(requested) {
		return 0, linuxUnsupported(operation, "filesystem omitted allocation or identity", nil)
	}
	observed := linuxOpenObjectIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, mode: stat.Mode,
	}
	if !linuxSameNamespaceObject(entry.object, observed) {
		return 0, linuxUnsafe(operation, "allocation metadata differs from the pinned object", nil)
	}
	const statBlockBytes = uint64(512)
	if stat.Blocks > math.MaxUint64/statBlockBytes {
		return 0, linuxUnsafe(operation, "allocated byte count overflows", nil)
	}
	return stat.Blocks * statBlockBytes, nil
}

func (directory *linuxOutputDirectory) pinnedEntryMatches(
	name string,
	expected *linuxOutputPinnedEntry,
) (bool, error) {
	const operation = "compare pinned output entry"
	if expected == nil || expected.fd < 0 || expected.system != directory.system ||
		expected.certificate.mount != directory.certificate.mount {
		return false, linuxUnsafe(operation, "pinned entry belongs to incompatible authority", nil)
	}
	current, err := directory.openPinnedEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	same := expected.kind == current.kind && expected.object.sameObject(current.object)
	return same, errors.Join(current.close())
}

func (directory *linuxOutputDirectory) openPinnedDirectory(
	expected *linuxOutputPinnedEntry,
	private bool,
) (*linuxOutputDirectory, error) {
	const operation = "open pinned output directory"
	if expected == nil || expected.kind != outputV3EntryDirectory || expected.fd < 0 {
		return nil, linuxUnsafe(operation, "pinned entry is not an open directory", nil)
	}
	var opened *linuxOutputDirectory
	var err error
	if private {
		opened, err = directory.openDirectoryExact(expected.name, linuxOutputDirectoryMode)
	} else {
		opened, err = directory.openDirectory(expected.name)
	}
	if err != nil {
		return nil, err
	}
	if !expected.object.sameObject(opened.object) {
		return nil, errors.Join(
			linuxUnsafe(operation, "opened directory differs from the pinned entry", nil),
			opened.close(),
		)
	}
	return opened, nil
}

func (directory *linuxOutputDirectory) removePinnedEntry(
	name string,
	expected *linuxOutputPinnedEntry,
) (resultErr error) {
	const operation = "remove pinned output entry"
	if expected == nil || expected.fd < 0 {
		return linuxUnsafe(operation, "pinned entry authority is absent", nil)
	}
	current, err := directory.openPinnedEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.close()) }()
	if expected.kind != current.kind || !expected.object.sameObject(current.object) {
		return linuxUnsafe(operation, "current name differs from the pinned entry", nil)
	}
	flags := 0
	if current.kind == outputV3EntryDirectory {
		flags = unix.AT_REMOVEDIR
	}
	if err := directory.system.unlinkat(directory.fd, name, flags); err != nil {
		return fmt.Errorf("%s %q: %w", operation, name, err)
	}
	return directory.sync()
}
