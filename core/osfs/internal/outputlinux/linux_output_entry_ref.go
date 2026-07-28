//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

type linuxOutputPinnedEntry struct {
	system      *linuxOutputSystem
	fd          int
	certificate linuxOutputCertificate
	object      linuxOpenHandleIdentity
	kind        outputcap.EntryKind
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
	before, err := directory.namedEntrySnapshotNoFollow(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	flags := unix.O_PATH
	switch before.identity.kind {
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
	after, err := directory.namedEntrySnapshotNoFollow(name)
	if err != nil {
		return nil, err
	}
	if !before.matches(opened.identity) || !after.matches(opened.identity) {
		return nil, linuxUnsafe(operation, "entry changed while its no-follow handle was pinned", nil)
	}
	return &linuxOutputPinnedEntry{
		system: directory.system, fd: fd, certificate: directory.certificate,
		object: opened.identity, kind: linuxOutputEntryKind(opened.mode), name: name,
	}, nil
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
	if !entry.object.sameObject(current.identity) || entry.object.kind != current.identity.kind {
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
	observed := linuxOpenHandleIdentity{
		mountID: stat.Mnt_id, deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor,
		inode: stat.Ino, kind: linuxFileType(stat.Mode),
	}
	if !entry.object.sameObject(observed) {
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
	if expected == nil || expected.kind != outputcap.EntryDirectory || expected.fd < 0 {
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
	if current.kind == outputcap.EntryDirectory {
		flags = unix.AT_REMOVEDIR
	}
	if err := directory.system.unlinkat(directory.fd, name, flags); err != nil {
		return fmt.Errorf("%s %q: %w", operation, name, err)
	}
	return directory.sync()
}
