//go:build linux

package outputlinux

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const linuxNamespaceCoverageMountID = uint64(0x57534e53)

var linuxNamespaceCoverageFilesystemID = [2]int32{0x5753, 0x4e53}

type linuxNamespaceCoverageHarness struct {
	root       *linuxOutputDirectory
	path       string
	closeCount int
	baseStatx  func(int, string, int, int, *unix.Statx_t) error
}

func TestLinuxNamespaceNoReplaceAndExplicitCleanup(t *testing.T) {
	harness := newLinuxNamespaceCoverageHarness(t)
	root := harness.root
	source := linuxNamespaceCoverageCreateFile(t, root, "source", 8)
	target := linuxNamespaceCoverageCreateFile(t, root, "target", 4)

	if err := root.renameRegularFile("source", source, root, "target", linuxRenameNoReplace); !errors.Is(err, errLinuxOutputCollision) {
		t.Fatalf("no-replace rename error = %v", err)
	}
	linuxNamespaceCoverageAssertRegularName(t, root, "source", source)
	linuxNamespaceCoverageAssertRegularName(t, root, "target", target)

	if err := root.renameRegularFile("source", source, root, "moved", linuxRenameNoReplace); err != nil {
		t.Fatalf("no-replace rename: %v", err)
	}
	linuxNamespaceCoverageAssertMissing(t, harness.path, "source")
	linuxNamespaceCoverageAssertRegularName(t, root, "moved", source)

	if err := root.linkRegularFileNoReplace(root, "moved", source, "published"); err != nil {
		t.Fatalf("no-replace link: %v", err)
	}
	if err := root.linkRegularFileNoReplace(root, "moved", source, "published"); !errors.Is(err, errLinuxOutputCollision) {
		t.Fatalf("second no-replace link error = %v", err)
	}
	linuxNamespaceCoverageAssertRegularName(t, root, "published", source)

	sourceDirectory := linuxNamespaceCoverageCreateDirectory(t, root, "source-directory")
	targetDirectory := linuxNamespaceCoverageCreateDirectory(t, root, "target-directory")
	if err := root.renameDirectory(
		"source-directory", sourceDirectory, root, "target-directory", linuxRenameNoReplace,
	); !errors.Is(err, errLinuxOutputCollision) {
		t.Fatalf("directory no-replace rename error = %v", err)
	}
	if err := root.renameDirectory(
		"source-directory", sourceDirectory, root, "moved-directory", linuxRenameNoReplace,
	); err != nil {
		t.Fatalf("directory no-replace rename: %v", err)
	}

	if err := root.unlinkRegularFile("published", source); err != nil {
		t.Fatalf("remove published link: %v", err)
	}
	if err := root.unlinkRegularFile("moved", source); err != nil {
		t.Fatalf("remove moved source: %v", err)
	}
	if err := root.unlinkRegularFile("target", target); err != nil {
		t.Fatalf("remove collision target: %v", err)
	}
	if err := root.unlinkDirectory("moved-directory", sourceDirectory); err != nil {
		t.Fatalf("remove moved directory: %v", err)
	}
	if err := root.unlinkDirectory("target-directory", targetDirectory); err != nil {
		t.Fatalf("remove target directory: %v", err)
	}
	linuxNamespaceCoverageAssertEmpty(t, harness.path)
}

func TestLinuxNamespaceIdentityMismatchFailsClosed(t *testing.T) {
	harness := newLinuxNamespaceCoverageHarness(t)
	root := harness.root
	original := linuxNamespaceCoverageCreateFile(t, root, "victim", 1)
	if err := root.renameRegularFile("victim", original, root, "displaced", linuxRenameReplace); err != nil {
		t.Fatalf("displace original: %v", err)
	}
	replacement := linuxNamespaceCoverageCreateFile(t, root, "victim", 2)

	if err := root.unlinkRegularFile("victim", original); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("unlink replacement with stale authority error = %v", err)
	}
	if err := root.renameRegularFile("victim", original, root, "renamed", linuxRenameReplace); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("rename replacement with stale authority error = %v", err)
	}
	if err := root.linkRegularFileNoReplace(root, "victim", original, "linked"); !errors.Is(err, outputcap.ErrFixedLinkSourceChanged) || !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("link replacement with stale authority error = %v", err)
	}
	linuxNamespaceCoverageAssertRegularName(t, root, "victim", replacement)
	linuxNamespaceCoverageAssertMissing(t, harness.path, "renamed")
	linuxNamespaceCoverageAssertMissing(t, harness.path, "linked")

	originalDirectory := linuxNamespaceCoverageCreateDirectory(t, root, "victim-directory")
	if err := root.renameDirectory(
		"victim-directory", originalDirectory, root, "displaced-directory", linuxRenameReplace,
	); err != nil {
		t.Fatalf("displace original directory: %v", err)
	}
	replacementDirectory := linuxNamespaceCoverageCreateDirectory(t, root, "victim-directory")
	if err := root.unlinkDirectory("victim-directory", originalDirectory); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("unlink replacement directory with stale authority error = %v", err)
	}
	if err := root.renameDirectory(
		"victim-directory", originalDirectory, root, "renamed-directory", linuxRenameReplace,
	); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("rename replacement directory with stale authority error = %v", err)
	}

	if err := root.unlinkRegularFile("victim", replacement); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkRegularFile("displaced", original); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkDirectory("victim-directory", replacementDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkDirectory("displaced-directory", originalDirectory); err != nil {
		t.Fatal(err)
	}
	linuxNamespaceCoverageAssertEmpty(t, harness.path)
}

func TestLinuxPinnedEntryIdentityAndCleanup(t *testing.T) {
	harness := newLinuxNamespaceCoverageHarness(t)
	root := harness.root
	file := linuxNamespaceCoverageCreateFile(t, root, "pinned-file", 4096)
	pinned, err := root.openPinnedEntry("pinned-file")
	if err != nil {
		t.Fatalf("pin file: %v", err)
	}
	t.Cleanup(func() { _ = pinned.close() })
	matches, err := root.pinnedEntryMatches("pinned-file", pinned)
	if err != nil || !matches {
		t.Fatalf("pinned match = %t, error = %v", matches, err)
	}
	if err := root.renameRegularFile("pinned-file", file, root, "old-pinned-file", linuxRenameReplace); err != nil {
		t.Fatalf("move pinned file: %v", err)
	}
	replacement := linuxNamespaceCoverageCreateFile(t, root, "pinned-file", 1)
	matches, err = root.pinnedEntryMatches("pinned-file", pinned)
	if err != nil || matches {
		t.Fatalf("replacement match = %t, error = %v", matches, err)
	}
	if err := root.removePinnedEntry("pinned-file", pinned); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("remove replacement with stale pin error = %v", err)
	}
	linuxNamespaceCoverageAssertRegularName(t, root, "pinned-file", replacement)

	directory := linuxNamespaceCoverageCreateDirectory(t, root, "pinned-directory")
	pinnedDirectory, err := root.openPinnedEntry("pinned-directory")
	if err != nil {
		t.Fatalf("pin directory: %v", err)
	}
	t.Cleanup(func() { _ = pinnedDirectory.close() })
	opened, err := root.openPinnedDirectory(pinnedDirectory, true)
	if err != nil {
		t.Fatalf("open pinned private directory: %v", err)
	}
	if err := opened.close(); err != nil {
		t.Fatalf("close pinned private directory: %v", err)
	}
	if err := root.renameDirectory(
		"pinned-directory", directory, root, "old-pinned-directory", linuxRenameReplace,
	); err != nil {
		t.Fatalf("move pinned directory: %v", err)
	}
	replacementDirectory := linuxNamespaceCoverageCreateDirectory(t, root, "pinned-directory")
	closesBefore := harness.closeCount
	if opened, err = root.openPinnedDirectory(pinnedDirectory, false); !errors.Is(err, errLinuxOutputUnsafe) || opened != nil {
		t.Fatalf("open replacement with stale directory pin = %v, %v", opened, err)
	}
	if harness.closeCount != closesBefore+1 {
		t.Fatalf("identity-mismatched opened directory closes = %d, want %d", harness.closeCount, closesBefore+1)
	}

	removable := linuxNamespaceCoverageCreateFile(t, root, "removable", 0)
	removablePin, err := root.openPinnedEntry("removable")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.removePinnedEntry("removable", removablePin); err != nil {
		t.Fatalf("remove pinned file: %v", err)
	}
	if err := root.removePinnedEntry("removable", removablePin); err != nil {
		t.Fatalf("repeat missing pinned removal: %v", err)
	}
	if err := removablePin.close(); err != nil {
		t.Fatal(err)
	}
	if err := removable.close(); err != nil {
		t.Fatal(err)
	}
	if err := root.removePinnedEntry("absent", nil); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("remove without pin error = %v", err)
	}

	if err := root.unlinkRegularFile("pinned-file", replacement); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkRegularFile("old-pinned-file", file); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkDirectory("pinned-directory", replacementDirectory); err != nil {
		t.Fatal(err)
	}
	if err := root.unlinkDirectory("old-pinned-directory", directory); err != nil {
		t.Fatal(err)
	}
	linuxNamespaceCoverageAssertEmpty(t, harness.path)
}

func TestLinuxCreateAuthorityFailurePrecedesNamespaceMutation(t *testing.T) {
	tests := []struct {
		name      string
		directory bool
		want      error
		configure func(*linuxNamespaceCoverageHarness)
	}{
		{
			name: "provider absent", want: errLinuxOutputUnsupported,
			configure: func(harness *linuxNamespaceCoverageHarness) { harness.root.system.faccessat2 = nil },
		},
		{
			name: "effective access denied", directory: true, want: errLinuxOutputUnsafe,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.faccessat2 = func(int, string, uint32, int) error { return unix.EACCES }
			},
		},
		{
			name: "effective access unavailable", want: errLinuxOutputUnsupported,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.faccessat2 = func(int, string, uint32, int) error { return unix.ENOSYS }
			},
		},
		{
			name: "setgid inheritance", directory: true, want: errLinuxOutputUnsupported,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.statx = linuxNamespaceCoverageMutateStatx(
					harness.baseStatx, func(stat *unix.Statx_t) { stat.Mode |= unix.S_ISGID },
				)
			},
		},
		{
			name: "default ACL unreadable", want: errLinuxOutputUnsafe,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.fgetxattr = func(int, string, []byte) (int, error) { return 0, unix.EIO }
			},
		},
		{
			name: "foreign owner", directory: true, want: errLinuxOutputUnsafe,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.statx = linuxNamespaceCoverageMutateStatx(
					harness.baseStatx, func(stat *unix.Statx_t) { stat.Uid++ },
				)
			},
		},
		{
			name: "external child mutation authority", want: errLinuxOutputUnsafe,
			configure: func(harness *linuxNamespaceCoverageHarness) {
				harness.root.system.statx = linuxNamespaceCoverageMutateStatx(
					harness.baseStatx,
					func(stat *unix.Statx_t) { stat.Mode = stat.Mode&^0o777 | unix.S_IFDIR | 0o730 },
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newLinuxNamespaceCoverageHarness(t)
			root := harness.root
			originalOpenat2 := root.system.openat2
			originalMkdirat := root.system.mkdirat
			mutations := 0
			root.system.openat2 = func(fd int, name string, how *unix.OpenHow) (int, error) {
				if how.Flags&unix.O_CREAT != 0 {
					mutations++
				}
				return originalOpenat2(fd, name, how)
			}
			root.system.mkdirat = func(fd int, name string, mode uint32) error {
				mutations++
				return originalMkdirat(fd, name, mode)
			}
			test.configure(harness)

			var err error
			if test.directory {
				_, err = root.createPrivateDirectoryExact("blocked", linuxOutputDirectoryMode)
			} else {
				_, err = root.createRegularFileExact("blocked", linuxOutputStateFileMode, 0)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("authority error = %v, want %v", err, test.want)
			}
			if mutations != 0 {
				t.Fatalf("authority failure allowed %d namespace mutations", mutations)
			}
			linuxNamespaceCoverageAssertMissing(t, harness.path, "blocked")
		})
	}
}

func newLinuxNamespaceCoverageHarness(t *testing.T) *linuxNamespaceCoverageHarness {
	t.Helper()
	path := t.TempDir()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("open test root: %v", err)
	}
	harness := &linuxNamespaceCoverageHarness{path: path}
	system := linuxHostOutputSystem
	// The namespace primitives remain real kernel operations. Virtualizing only
	// certification facts keeps these tests unprivileged and independent of the
	// filesystem that backs a GitHub runner's temporary directory.
	system.statx = linuxNamespaceCoverageStatx
	harness.baseStatx = system.statx
	system.fstatfs = func(_ int, stat *unix.Statfs_t) error {
		*stat = unix.Statfs_t{}
		reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(linuxExt4SuperMagic)
		stat.Fsid.Val = linuxNamespaceCoverageFilesystemID
		return nil
	}
	system.getFlags = func(int) (uint32, error) { return 0, nil }
	system.fsync = func(int) error { return nil }
	system.faccessat2 = func(int, string, uint32, int) error { return nil }
	system.fgetxattr = func(int, string, []byte) (int, error) { return 0, unix.ENODATA }
	system.geteuid = unix.Geteuid
	system.close = func(fd int) error {
		harness.closeCount++
		return unix.Close(fd)
	}
	facts, err := linuxReadOpenHandleFacts(&system, fd, unix.STATX_MNT_ID_UNIQUE)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("read test root identity: %v", err)
	}
	certificate := linuxOutputCertificate{
		mount: linuxMountIdentity{
			uniqueMountID: linuxNamespaceCoverageMountID,
			deviceMajor:   facts.identity.deviceMajor, deviceMinor: facts.identity.deviceMinor,
			runtimeFilesystemID: linuxNamespaceCoverageFilesystemID,
		},
		rootObject: facts.identity,
		durability: linuxOutputProcessRestartDurability,
	}
	harness.root = &linuxOutputDirectory{
		system: &system, fd: fd, certificate: certificate, object: facts.identity,
	}
	t.Cleanup(func() {
		if err := harness.root.close(); err != nil {
			t.Errorf("close test root: %v", err)
		}
	})
	return harness
}

func linuxNamespaceCoverageStatx(
	fd int,
	name string,
	flags int,
	mask int,
	stat *unix.Statx_t,
) error {
	var native unix.Stat_t
	var err error
	if name == "" {
		err = unix.Fstat(fd, &native)
	} else {
		err = unix.Fstatat(fd, name, &native, flags&unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != nil {
		return err
	}
	*stat = unix.Statx_t{
		Mask: uint32(mask), Ino: native.Ino, Mode: uint16(native.Mode), Uid: native.Uid,
		Dev_major: uint32(unix.Major(uint64(native.Dev))),
		Dev_minor: uint32(unix.Minor(uint64(native.Dev))),
		Mnt_id:    linuxNamespaceCoverageMountID,
	}
	if native.Size > 0 {
		stat.Size = uint64(native.Size)
	}
	if native.Blocks > 0 {
		stat.Blocks = uint64(native.Blocks)
	}
	return nil
}

func linuxNamespaceCoverageMutateStatx(
	base func(int, string, int, int, *unix.Statx_t) error,
	mutate func(*unix.Statx_t),
) func(int, string, int, int, *unix.Statx_t) error {
	return func(fd int, name string, flags int, mask int, stat *unix.Statx_t) error {
		if err := base(fd, name, flags, mask, stat); err != nil {
			return err
		}
		mutate(stat)
		return nil
	}
}

func linuxNamespaceCoverageCreateFile(
	t *testing.T,
	directory *linuxOutputDirectory,
	name string,
	size int64,
) *linuxOutputRegularFile {
	t.Helper()
	file, err := directory.createRegularFileExact(name, linuxOutputStateFileMode, size)
	if err != nil {
		t.Fatalf("create file %q: %v", name, err)
	}
	t.Cleanup(func() { _ = file.close() })
	return file
}

func linuxNamespaceCoverageCreateDirectory(
	t *testing.T,
	directory *linuxOutputDirectory,
	name string,
) *linuxOutputDirectory {
	t.Helper()
	created, err := directory.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
	if err != nil {
		t.Fatalf("create directory %q: %v", name, err)
	}
	t.Cleanup(func() { _ = created.close() })
	return created
}

func linuxNamespaceCoverageAssertRegularName(
	t *testing.T,
	directory *linuxOutputDirectory,
	name string,
	expected *linuxOutputRegularFile,
) {
	t.Helper()
	matches, err := directory.regularEntryMatches(name, expected)
	if err != nil || !matches {
		t.Fatalf("regular name %q match = %t, error = %v", name, matches, err)
	}
}

func linuxNamespaceCoverageAssertMissing(t *testing.T, directory, name string) {
	t.Helper()
	if _, err := os.Lstat(directory + string(os.PathSeparator) + name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entry %q still exists or cannot be inspected: %v", name, err)
	}
}

func linuxNamespaceCoverageAssertEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("test namespace retained %d entries", len(entries))
	}
}
