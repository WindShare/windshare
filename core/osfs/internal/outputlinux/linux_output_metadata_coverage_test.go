//go:build linux

package outputlinux

import (
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/unix"
)

func mustModifiedTime(t *testing.T, seconds int64, nanoseconds uint32, precision catalog.TimePrecision) catalog.ModifiedTime {
	t.Helper()
	m, err := catalog.NewModifiedTime(seconds, nanoseconds, precision)
	if err != nil {
		t.Fatalf("mustModifiedTime(%d, %d, %d): %v", seconds, nanoseconds, precision, err)
	}
	return m
}

func TestLinuxValidateModifiedTimeAndTimespecConversion(t *testing.T) {
	// Unset modified time
	unset := catalog.ModifiedTime{}
	if err := linuxValidateModifiedTime(unset); err != nil {
		t.Fatalf("unset modified time error = %v", err)
	}
	if linuxModifiedTimeRequiresExtendedInodeFields(unset) {
		t.Fatal("unset modified time required extended inode fields")
	}

	// Extended inode fields requirement
	baseSec := mustModifiedTime(t, 100, 0, catalog.TimePrecisionSeconds)
	if linuxModifiedTimeRequiresExtendedInodeFields(baseSec) {
		t.Fatal("standard seconds required extended inode fields")
	}
	if err := linuxValidateModifiedTime(baseSec); err != nil {
		t.Fatalf("valid seconds modified time error = %v", err)
	}

	withNs := mustModifiedTime(t, 100, 500, catalog.TimePrecisionNanoseconds)
	if !linuxModifiedTimeRequiresExtendedInodeFields(withNs) {
		t.Fatal("nanoseconds did not require extended inode fields")
	}
	if err := linuxValidateModifiedTime(withNs); err != nil {
		t.Fatalf("valid nanoseconds modified time error = %v", err)
	}

	epochOverflow := mustModifiedTime(t, math.MaxInt32+1, 0, catalog.TimePrecisionSeconds)
	if !linuxModifiedTimeRequiresExtendedInodeFields(epochOverflow) {
		t.Fatal("epoch overflow did not require extended inode fields")
	}
	if err := linuxValidateModifiedTime(epochOverflow); err != nil {
		t.Fatalf("epoch overflow modified time error = %v", err)
	}

	epochUnderflow := mustModifiedTime(t, math.MinInt32-1, 0, catalog.TimePrecisionSeconds)
	if !linuxModifiedTimeRequiresExtendedInodeFields(epochUnderflow) {
		t.Fatal("epoch underflow did not require extended inode fields")
	}
	if err := linuxValidateModifiedTime(epochUnderflow); err != nil {
		t.Fatalf("epoch underflow modified time error = %v", err)
	}

	// Modified time matching
	meta := linuxOutputMetadata{size: 100, seconds: 200, nanoseconds: 300_400_500}
	if !linuxModifiedTimeMatches(meta, unset) {
		t.Fatal("unset expected time did not match")
	}
	if linuxModifiedTimeMatches(meta, mustModifiedTime(t, 201, 0, catalog.TimePrecisionSeconds)) {
		t.Fatal("seconds mismatch matched")
	}
	if !linuxModifiedTimeMatches(meta, mustModifiedTime(t, 200, 0, catalog.TimePrecisionSeconds)) {
		t.Fatal("seconds precision match failed")
	}
	if !linuxModifiedTimeMatches(meta, mustModifiedTime(t, 200, 300_000_000, catalog.TimePrecisionMilliseconds)) {
		t.Fatal("milliseconds precision match failed")
	}
	if linuxModifiedTimeMatches(meta, mustModifiedTime(t, 200, 301_000_000, catalog.TimePrecisionMilliseconds)) {
		t.Fatal("milliseconds precision mismatch matched")
	}
	if !linuxModifiedTimeMatches(meta, mustModifiedTime(t, 200, 300_400_500, catalog.TimePrecisionNanoseconds)) {
		t.Fatal("nanoseconds precision match failed")
	}
	if linuxModifiedTimeMatches(meta, mustModifiedTime(t, 200, 300_400_501, catalog.TimePrecisionNanoseconds)) {
		t.Fatal("nanoseconds precision mismatch matched")
	}
}

func TestLinuxSetHandleModifiedTimeAndErrors(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)

	// Unset modified time is a no-op
	if err := linuxSetHandleModifiedTime(root.system, root.fd, catalog.ModifiedTime{}, "test"); err != nil {
		t.Fatalf("unset modified time set error = %v", err)
	}

	// utimensat errors
	validTime := mustModifiedTime(t, 100, 500, catalog.TimePrecisionNanoseconds)
	for _, sysErr := range []error{unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP} {
		root.system.utimensat = func(int, string, []unix.Timespec, int) error { return sysErr }
		if err := linuxSetHandleModifiedTime(root.system, root.fd, validTime, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("utimensat error %v produced %v, want unsupported", sysErr, err)
		}
	}
	root.system.utimensat = func(int, string, []unix.Timespec, int) error { return unix.EIO }
	if err := linuxSetHandleModifiedTime(root.system, root.fd, validTime, "test"); err == nil || errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("utimensat generic error produced %v", err)
	}

	// utimensat success
	root.system.utimensat = func(int, string, []unix.Timespec, int) error { return nil }
	if err := linuxSetHandleModifiedTime(root.system, root.fd, validTime, "test"); err != nil {
		t.Fatalf("valid set modified time error = %v", err)
	}

	// Directory and Regular file setModifiedTime
	file := &linuxOutputRegularFile{
		system: root.system, fd: linuxAuthorityRegularFileFD, certificate: root.certificate,
		object: linuxOpenHandleIdentity{
			mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
			deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode + 1,
			kind: unix.S_IFREG,
		},
		writable: true,
	}

	// File not writable
	file.writable = false
	if err := file.setModifiedTime(validTime); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("read-only file setModifiedTime error = %v, want unsafe", err)
	}
	file.writable = true

	if err := file.setModifiedTime(validTime); err != nil {
		t.Fatalf("file setModifiedTime error = %v", err)
	}
	if err := root.setModifiedTime(validTime); err != nil {
		t.Fatalf("directory setModifiedTime error = %v", err)
	}

	// metadataMatches
	matched, err := file.metadataMatches(0, validTime)
	if err != nil || !matched {
		t.Fatalf("file metadataMatches error=%v matched=%v", err, matched)
	}
	_ = harness
}

func TestLinuxRequireExtendedTimestampLayoutBranches(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)

	// nil statx
	origStatx := root.system.statx
	root.system.statx = nil
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil statx error = %v, want unsupported", err)
	}
	root.system.statx = origStatx

	// Wrong object type
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFREG, "test"); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("wrong object type error = %v, want unsafe", err)
	}

	// nil geteuid
	root.system.geteuid = nil
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("nil geteuid error = %v, want unsupported", err)
	}
	root.system.geteuid = func() int { return 0 }

	// UID mismatch
	harness.ownerUID = 1000
	root.system.geteuid = func() int { return 2000 }
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("UID mismatch error = %v, want unsafe", err)
	}
	harness.ownerUID = 0
	root.system.geteuid = func() int { return 0 }

	// statx errors
	for _, sysErr := range []error{unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP} {
		root.system.statx = func(int, string, int, int, *unix.Statx_t) error { return sysErr }
		if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("statx error %v produced %v, want unsupported", sysErr, err)
		}
	}
	root.system.statx = func(int, string, int, int, *unix.Statx_t) error { return unix.EIO }
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); err == nil || errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("statx generic error produced %v", err)
	}

	// statx missing identity mask
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Mask &^= unix.STATX_INO
		return nil
	}
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("missing STATX_INO error = %v, want unsupported", err)
	}

	// statx missing STATX_BTIME
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Mask &^= unix.STATX_BTIME
		return nil
	}
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("missing STATX_BTIME error = %v, want unsupported", err)
	}

	// statx identity changed
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Ino = 99999
		return nil
	}
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("changed inode error = %v, want unsafe", err)
	}

	// statx Btime.Nsec invalid
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Btime.Nsec = 1_000_000_000
		return nil
	}
	if err := linuxRequireExtendedTimestampLayout(root.system, root.fd, root.certificate, unix.S_IFDIR, "test"); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("invalid Btime.Nsec error = %v, want unsafe", err)
	}
}

func TestLinuxReadHandleMetadataAndErrors(t *testing.T) {
	root, _ := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	origStatx := root.system.statx

	// statx failure
	root.system.statx = func(int, string, int, int, *unix.Statx_t) error { return unix.EIO }
	if _, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFDIR); err == nil {
		t.Fatal("expected error on statx EIO")
	}

	// missing mask
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Mask &^= unix.STATX_SIZE
		return nil
	}
	if _, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFDIR); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("missing STATX_SIZE error = %v, want unsupported", err)
	}

	// mount mismatch
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Mnt_id = 9999
		return nil
	}
	if _, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFDIR); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("mount mismatch error = %v, want unsafe", err)
	}

	// wrong object kind
	root.system.statx = origStatx
	if _, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFREG); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("wrong kind error = %v, want unsafe", err)
	}

	// invalid mtime
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Mtime.Nsec = 1_000_000_000
		return nil
	}
	if _, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFDIR); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("invalid mtime.Nsec error = %v, want unsafe", err)
	}

	// valid metadata read
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Size = 4096
		stat.Mtime.Sec = 100
		stat.Mtime.Nsec = 200
		return nil
	}
	meta, err := linuxReadHandleMetadata(root.system, root.fd, root.certificate, unix.S_IFDIR)
	if err != nil {
		t.Fatalf("valid metadata error = %v", err)
	}
	if meta.size != 4096 || meta.seconds != 100 || meta.nanoseconds != 200 {
		t.Fatalf("unexpected metadata values: %+v", meta)
	}
}

func TestLinuxHandlesExactModeTruncateSyncAndPinnedEntry(t *testing.T) {
	root, harness := newLinuxAuthorityRoot(t)
	installLinuxSafeAuthorityHarness(root.system)
	closed := false
	root.system.close = func(int) error {
		closed = true
		return nil
	}

	// PinnedEntry close
	var nilPinned *linuxOutputPinnedEntry
	if err := nilPinned.close(); err != nil {
		t.Fatalf("nil pinned close error: %v", err)
	}
	pinnedClosed := &linuxOutputPinnedEntry{fd: -1}
	if err := pinnedClosed.close(); err != nil {
		t.Fatalf("closed pinned close error: %v", err)
	}
	pinned := &linuxOutputPinnedEntry{
		system: root.system, fd: 42,
	}
	if err := pinned.close(); err != nil {
		t.Fatalf("pinned close error: %v", err)
	}
	if !closed || pinned.fd != -1 {
		t.Fatalf("pinned close failed: closed=%v fd=%d", closed, pinned.fd)
	}

	// Regular file operations
	file := &linuxOutputRegularFile{
		system: root.system, fd: linuxAuthorityRegularFileFD, certificate: root.certificate,
		object: linuxOpenHandleIdentity{
			mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
			deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode + 1,
			kind: unix.S_IFREG,
		},
		writable: true,
	}

	// setExactMode
	root.system.fchmod = func(int, uint32) error { return nil }
	if err := file.setExactMode(linuxOutputStateFileMode); err != nil {
		t.Fatalf("file setExactMode error = %v", err)
	}
	if err := root.setExactMode(linuxOutputDirectoryMode); err != nil {
		t.Fatalf("directory setExactMode error = %v", err)
	}

	// truncate
	if err := file.truncate(-1); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("negative truncate error = %v, want unsafe", err)
	}
	root.system.ftruncate = func(int, int64) error { return nil }
	origStatx := root.system.statx
	root.system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
		_ = origStatx(fd, path, flags, mask, stat)
		stat.Size = 1024
		return nil
	}
	if err := file.truncate(1024); err != nil {
		t.Fatalf("valid truncate error = %v", err)
	}

	// sync
	root.system.fsync = func(int) error { return nil }
	if err := file.sync(); err != nil {
		t.Fatalf("file sync error = %v", err)
	}
	if err := root.sync(); err != nil {
		t.Fatalf("directory sync error = %v", err)
	}

	for _, sysErr := range []error{unix.EINVAL, unix.ENOSYS, unix.EOPNOTSUPP} {
		root.system.fsync = func(int) error { return sysErr }
		if err := file.sync(); !errors.Is(err, errLinuxOutputUnsupported) {
			t.Fatalf("fsync %v produced %v, want unsupported", sysErr, err)
		}
	}
	_ = harness
}
