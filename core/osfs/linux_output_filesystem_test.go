//go:build linux

package osfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/unix"
)

const (
	linuxTestLegacyMountID = 41
	linuxTestUniqueMountID = 9001
	linuxTestDeviceMajor   = 8
	linuxTestDeviceMinor   = 2
	linuxTestRootInode     = 73
	linuxTestGeneration    = 19
	linuxTestBirthSeconds  = 1_722_000_000
	linuxTestBirthNanos    = 123_456_789
)

var linuxTestFilesystemUUID = [linuxFilesystemUUIDBytes]byte{
	0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87,
	0x98, 0xa9, 0xba, 0xcb, 0xdc, 0xed, 0xfe, 0x0f,
}

func TestLinuxFindMountInfo(t *testing.T) {
	t.Parallel()
	data := []byte("40 1 0:5 / /proc rw,nosuid - proc proc rw\n" +
		"41 1 8:2 / /srv/output rw,relatime shared:9 - ext4 /dev/sda2 rw,errors=remount-ro\n")
	record, err := linuxFindMountInfo(data, linuxTestLegacyMountID)
	if err != nil {
		t.Fatalf("find mountinfo: %v", err)
	}
	if record.mountID != linuxTestLegacyMountID || record.deviceMajor != linuxTestDeviceMajor ||
		record.deviceMinor != linuxTestDeviceMinor || record.filesystemType != "ext4" {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestLinuxFindMountInfoRejectsAmbiguityAndMalformedInput(t *testing.T) {
	t.Parallel()
	tests := map[string][]byte{
		"missing":   []byte("40 1 0:5 / /proc rw - proc proc rw\n"),
		"duplicate": []byte("41 1 8:2 / /a rw - ext4 /dev/sda2 rw\n41 1 8:2 / /b rw - ext4 /dev/sda2 rw\n"),
		"malformed": []byte("41 malformed\n"),
	}
	for name, data := range tests {
		data := data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := linuxFindMountInfo(data, linuxTestLegacyMountID); err == nil {
				t.Fatal("expected mountinfo failure")
			}
		})
	}
}

func TestLinuxCertificationAcceptsOnlyExplicitExt4Mount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		filesystemMagic  int64
		mountType        string
		mountDeviceMajor uint32
		uniqueMask       bool
		wantUnsupported  bool
		wantUnsafe       bool
	}{
		{
			name:             "ext4",
			filesystemMagic:  linuxExt4SuperMagic,
			mountType:        "ext4",
			mountDeviceMajor: linuxTestDeviceMajor,
			uniqueMask:       true,
		},
		{
			name:             "ext3 sharing super magic",
			filesystemMagic:  linuxExt4SuperMagic,
			mountType:        "ext3",
			mountDeviceMajor: linuxTestDeviceMajor,
			uniqueMask:       true,
			wantUnsupported:  true,
		},
		{
			name:             "other superblock",
			filesystemMagic:  unix.TMPFS_MAGIC,
			mountType:        "tmpfs",
			mountDeviceMajor: 0,
			uniqueMask:       true,
			wantUnsupported:  true,
		},
		{
			name:             "mount table mismatch",
			filesystemMagic:  linuxExt4SuperMagic,
			mountType:        "ext4",
			mountDeviceMajor: linuxTestDeviceMajor + 1,
			uniqueMask:       true,
			wantUnsafe:       true,
		},
		{
			name:             "non-unique mount identity unavailable",
			filesystemMagic:  linuxExt4SuperMagic,
			mountType:        "ext4",
			mountDeviceMajor: linuxTestDeviceMajor,
			wantUnsupported:  true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(
				test.filesystemMagic,
				test.mountType,
				test.mountDeviceMajor,
				test.uniqueMask,
			)
			certificate, err := linuxCertifyExt4OutputFD(&system, 10)
			switch {
			case test.wantUnsupported:
				assertLinuxUnsupported(t, err)
			case test.wantUnsafe:
				assertLinuxUnsafe(t, err)
			case err != nil:
				t.Fatalf("certify ext4: %v", err)
			default:
				if certificate.durability != linuxOutputProcessRestartDurability {
					t.Fatalf("durability = %v", certificate.durability)
				}
				if certificate.mount.uniqueMountID != linuxTestUniqueMountID {
					t.Fatalf("unique mount ID = %d", certificate.mount.uniqueMountID)
				}
			}
		})
	}
}

func TestLinuxRejectedFilesystemClosesRootBeforeMutation(t *testing.T) {
	t.Parallel()
	system := linuxCertificationTestSystem(
		unix.TMPFS_MAGIC,
		"tmpfs",
		0,
		true,
	)
	opened := 0
	closed := 0
	mutations := 0
	system.openat2 = func(_ int, _ string, how *unix.OpenHow) (int, error) {
		opened++
		if how.Resolve&uint64(unix.RESOLVE_NO_SYMLINKS) == 0 {
			t.Error("root open did not reject symlinks")
		}
		return 10, nil
	}
	system.close = func(int) error {
		closed++
		return nil
	}
	system.mkdirat = func(int, string, uint32) error {
		mutations++
		return nil
	}
	_, err := linuxOpenExt4OutputRoot("/output", &system)
	assertLinuxUnsupported(t, err)
	if opened != 1 || closed != 1 || mutations != 0 {
		t.Fatalf("opened=%d closed=%d mutations=%d", opened, closed, mutations)
	}
}

func TestLinuxCertificationRequiresRestartIdentityAndByteExactDirectories(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*linuxOutputSystem)
	}{
		{
			name: "filesystem UUID unavailable",
			mutate: func(system *linuxOutputSystem) {
				system.getFilesystemUUID = nil
			},
		},
		{
			name: "zero filesystem UUID",
			mutate: func(system *linuxOutputSystem) {
				system.getFilesystemUUID = func(int) ([linuxFilesystemUUIDBytes]byte, error) {
					return [linuxFilesystemUUIDBytes]byte{}, nil
				}
			},
		},
		{
			name: "restart identity provider unavailable",
			mutate: func(system *linuxOutputSystem) {
				system.restartIdentity = nil
			},
		},
		{
			name: "birth time unavailable",
			mutate: func(system *linuxOutputSystem) {
				original := system.statx
				system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
					if err := original(fd, path, flags, mask, stat); err != nil {
						return err
					}
					stat.Mask &^= uint32(unix.STATX_BTIME)
					return nil
				}
			},
		},
		{
			name: "directory flag query unavailable",
			mutate: func(system *linuxOutputSystem) {
				system.getFlags = nil
			},
		},
		{
			name: "casefold directory",
			mutate: func(system *linuxOutputSystem) {
				system.getFlags = func(int) (uint32, error) { return linuxFSCasefoldFlag, nil }
			},
		},
		{
			name: "fscrypt directory",
			mutate: func(system *linuxOutputSystem) {
				system.getFlags = func(int) (uint32, error) { return linuxFSEncryptFlag, nil }
			},
		},
		{
			name: "project-inheriting directory",
			mutate: func(system *linuxOutputSystem) {
				system.getFlags = func(int) (uint32, error) { return linuxFSProjectInheritFlag, nil }
			},
		},
		{
			name: "process umask provider unavailable",
			mutate: func(system *linuxOutputSystem) {
				system.readProcessStatus = nil
			},
		},
		{
			name: "process umask missing",
			mutate: func(system *linuxOutputSystem) {
				system.readProcessStatus = func() ([]byte, error) { return []byte("Name:\ttest\n"), nil }
			},
		},
		{
			name: "process umask malformed",
			mutate: func(system *linuxOutputSystem) {
				system.readProcessStatus = func() ([]byte, error) { return []byte("Umask:\tinvalid\n"), nil }
			},
		},
		{
			name: "process umask masks owner bits",
			mutate: func(system *linuxOutputSystem) {
				system.readProcessStatus = func() ([]byte, error) { return []byte("Umask:\t0700\n"), nil }
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(
				linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
			)
			test.mutate(&system)
			_, err := linuxCertifyExt4OutputFD(&system, 10)
			assertLinuxUnsupported(t, err)
		})
	}
}

func TestLinuxCertificationTreatsGenerationAsOptionalEvidence(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*linuxOutputSystem){
		"provider unavailable": func(system *linuxOutputSystem) { system.getVersion = nil },
		"zero generation": func(system *linuxOutputSystem) {
			system.getVersion = func(int) (uint32, error) { return 0, nil }
		},
		"ioctl unsupported": func(system *linuxOutputSystem) {
			system.getVersion = func(int) (uint32, error) { return 0, unix.ENOTTY }
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(
				linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true,
			)
			mutate(&system)
			certificate, err := linuxCertifyExt4OutputFD(&system, 10)
			if err != nil {
				t.Fatalf("certify ext4: %v", err)
			}
			if certificate.rootRestartIdentity.hasGenerationProof {
				t.Fatal("optional generation evidence was recorded when unavailable or zero")
			}
		})
	}
}

func TestLinuxParseProcessUmask(t *testing.T) {
	t.Parallel()
	valid := map[string]uint32{
		"Name:\ttest\nUmask:\t0022\n": 0o022,
		"Umask: 0077\n":               0o077,
	}
	for encoded, want := range valid {
		if got, err := linuxParseProcessUmask([]byte(encoded)); err != nil || got != want {
			t.Errorf("parse %q = %#o, %v; want %#o", encoded, got, err, want)
		}
	}
	invalid := []string{
		"Name:\ttest\n",
		"Umask:\n",
		"Umask:\tinvalid\n",
		"Umask:\t1000\n",
		"Umask:\t0022\nUmask:\t0022\n",
	}
	for _, encoded := range invalid {
		if _, err := linuxParseProcessUmask([]byte(encoded)); err == nil {
			t.Errorf("invalid process status %q was accepted", encoded)
		}
	}
}

func TestLinuxRestartIdentityRejectsSameInodeWithDifferentBirthTime(t *testing.T) {
	t.Parallel()
	mount := linuxMountIdentity{
		uniqueMountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
		deviceMinor: linuxTestDeviceMinor, runtimeFilesystemID: [2]int32{17, 29},
		filesystemUUID: linuxTestFilesystemUUID,
	}
	first := linuxDirectoryRestartIdentity{
		mount: mount, inode: linuxTestRootInode, kind: unix.S_IFDIR,
		birthSeconds: linuxTestBirthSeconds, birthNanoseconds: linuxTestBirthNanos,
	}
	reused := first
	reused.birthNanoseconds++
	if first.sameDirectory(reused) {
		t.Fatal("same mount and inode with a new birth time was accepted as one directory")
	}
}

func TestLinuxRegularAllocationUsesLiveHandleIdentity(t *testing.T) {
	t.Parallel()
	const allocatedBlocks = uint64(7)
	allocationInode := uint64(linuxTestRootInode)
	system := linuxOutputSystem{
		statx: func(_ int, _ string, _ int, mask int, stat *unix.Statx_t) error {
			inode := uint64(linuxTestRootInode)
			blocks := uint64(0)
			if mask&unix.STATX_BLOCKS != 0 {
				inode = allocationInode
				blocks = allocatedBlocks
			}
			*stat = unix.Statx_t{
				Mask:      uint32(mask),
				Ino:       inode,
				Mode:      unix.S_IFREG | 0o600,
				Dev_major: linuxTestDeviceMajor,
				Dev_minor: linuxTestDeviceMinor,
				Mnt_id:    linuxTestUniqueMountID,
				Blocks:    blocks,
			}
			return nil
		},
		fstatfs: func(_ int, stat *unix.Statfs_t) error {
			reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(linuxExt4SuperMagic)
			stat.Fsid.Val = [2]int32{17, 29}
			return nil
		},
		getVersion: func(int) (uint32, error) { return linuxTestGeneration, nil },
	}
	certificate := linuxOutputCertificate{
		mount: linuxMountIdentity{
			uniqueMountID:       linuxTestUniqueMountID,
			deviceMajor:         linuxTestDeviceMajor,
			deviceMinor:         linuxTestDeviceMinor,
			runtimeFilesystemID: [2]int32{17, 29},
			filesystemUUID:      linuxTestFilesystemUUID,
		},
		durability: linuxOutputProcessRestartDurability,
	}
	file := linuxOutputRegularFile{
		system: &system, fd: 12, certificate: certificate,
		object: linuxOpenHandleIdentity{
			mountID: linuxTestUniqueMountID, deviceMajor: linuxTestDeviceMajor,
			deviceMinor: linuxTestDeviceMinor, inode: linuxTestRootInode,
			kind: unix.S_IFREG,
		},
	}
	allocated, err := file.allocatedSize()
	if err != nil || allocated != allocatedBlocks*512 {
		t.Fatalf("allocated bytes = %d, error = %v", allocated, err)
	}
	allocationInode++
	if _, err := file.allocatedSize(); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("allocation query accepted a different inode: %v", err)
	}
}

func TestLinuxRelativeOpenUsesAllResolutionBarriers(t *testing.T) {
	t.Parallel()
	var captured unix.OpenHow
	system := linuxOutputSystem{
		openat2: func(_ int, _ string, how *unix.OpenHow) (int, error) {
			captured = *how
			return 12, nil
		},
	}
	directory := linuxOutputDirectory{system: &system, fd: 11}
	fd, err := directory.openRelative("entry", unix.O_PATH, 0)
	if err != nil {
		t.Fatalf("open relative: %v", err)
	}
	if fd != 12 {
		t.Fatalf("fd = %d", fd)
	}
	wantResolve := uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS |
		unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV)
	if captured.Resolve != wantResolve {
		t.Fatalf("resolve = %#x, want %#x", captured.Resolve, wantResolve)
	}
	wantFlags := uint64(unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW)
	if captured.Flags != wantFlags {
		t.Fatalf("flags = %#x, want %#x", captured.Flags, wantFlags)
	}
}

func TestLinuxComponentValidation(t *testing.T) {
	t.Parallel()
	invalid := []string{"", ".", "..", "nested/name", "nul\x00name", strings.Repeat("a", linuxOutputNameMaximumBytes+1)}
	for _, name := range invalid {
		if err := linuxValidateComponent("validate", name); !errors.Is(err, errLinuxOutputUnsafe) {
			t.Errorf("name %q: expected unsafe error, got %v", name, err)
		}
	}
	if err := linuxValidateComponent("validate", "safe-name"); err != nil {
		t.Fatalf("safe name: %v", err)
	}
}

func TestLinuxModifiedTimespecRejectsSilentABITruncation(t *testing.T) {
	modified, err := catalog.NewModifiedTime(-1, 500_000_000, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	timespec, err := linuxModifiedTimespec(modified)
	if err != nil || int64(timespec.Sec) != -1 || int64(timespec.Nsec) != 500_000_000 {
		t.Fatalf("exact negative timestamp: timespec=%+v error=%v", timespec, err)
	}
	outside, err := catalog.NewModifiedTime(catalog.MaxSafeInteger, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linuxModifiedTimespec(outside); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("unrepresentable native timestamp was accepted: %v", err)
	}
}

func TestLinuxOpenErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cause  error
		target error
	}{
		{name: "unsupported kernel", cause: unix.ENOSYS, target: errLinuxOutputUnsupported},
		{name: "mount transition", cause: unix.EXDEV, target: errLinuxOutputUnsafe},
		{name: "symbolic link", cause: unix.ELOOP, target: errLinuxOutputUnsafe},
		{name: "ordinary io failure", cause: unix.EIO, target: unix.EIO},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := linuxClassifyOpenError("open", test.cause)
			if !errors.Is(err, test.target) {
				t.Fatalf("expected %v, got %v", test.target, err)
			}
		})
	}
}

func TestLinuxNativeNamespaceLifecycle(t *testing.T) {
	root, err := linuxOpenExt4OutputRoot(t.TempDir(), &linuxHostOutputSystem)
	if err != nil {
		if errors.Is(err, errLinuxOutputUnsupported) {
			t.Skipf("test host is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatalf("open certified root: %v", err)
	}
	t.Cleanup(func() { _ = root.close() })
	if root.durability() != linuxOutputProcessRestartDurability {
		t.Fatalf("durability = %v", root.durability())
	}
	if err := root.probeRecoverableFeatures(); err != nil {
		t.Fatalf("probe recoverable features: %v", err)
	}

	control, err := root.createPrivateDirectoryExact(".windshare-output", linuxOutputDirectoryMode)
	if err != nil {
		t.Fatalf("create control directory: %v", err)
	}
	t.Cleanup(func() { _ = control.close() })
	reopenedControl, err := root.openDirectoryExact(".windshare-output", linuxOutputDirectoryMode)
	if err != nil {
		t.Fatalf("reopen control directory: %v", err)
	}
	if !reopenedControl.object.sameObject(control.object) {
		t.Fatal("reopened control directory has different authority")
	}
	if err := reopenedControl.close(); err != nil {
		t.Fatalf("close reopened control directory: %v", err)
	}
	stage, err := control.createRegularFileExact("object.stage", linuxOutputStateFileMode, 4096)
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	t.Cleanup(func() { _ = stage.close() })

	if err := control.linkRegularFileNoReplace(control, "object.stage", stage, "object.anchor"); err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	anchor, err := control.openRegularFileExact("object.anchor", false, linuxOutputStateFileMode)
	if err != nil {
		t.Fatalf("open anchor: %v", err)
	}
	t.Cleanup(func() { _ = anchor.close() })
	same, err := linuxSameOpenRegularFile(stage, anchor)
	if err != nil || !same {
		t.Fatalf("stage/anchor relation: same=%v err=%v", same, err)
	}

	if err := root.linkRegularFileNoReplace(control, "object.anchor", anchor, "received.bin"); err != nil {
		t.Fatalf("publish final: %v", err)
	}
	if err := root.linkRegularFileNoReplace(control, "object.anchor", anchor, "received.bin"); !errors.Is(err, errLinuxOutputCollision) {
		t.Fatalf("second publication: expected collision, got %v", err)
	}
	final, err := root.openRegularFileExact("received.bin", false, linuxOutputStateFileMode)
	if err != nil {
		t.Fatalf("open final: %v", err)
	}
	t.Cleanup(func() { _ = final.close() })
	same, err = linuxSameOpenRegularFile(anchor, final)
	if err != nil || !same {
		t.Fatalf("anchor/final relation: same=%v err=%v", same, err)
	}

	oldRecord, err := control.createRegularFileExact("file.state", linuxOutputStateFileMode, 1)
	if err != nil {
		t.Fatalf("create old state record: %v", err)
	}
	t.Cleanup(func() { _ = oldRecord.close() })
	newRecord, err := control.createRegularFileExact("file.state.tmp", linuxOutputStateFileMode, 2)
	if err != nil {
		t.Fatalf("create new state record: %v", err)
	}
	t.Cleanup(func() { _ = newRecord.close() })
	if err := control.renameRegularFile(
		"file.state.tmp",
		newRecord,
		control,
		"file.state",
		linuxRenameReplace,
	); err != nil {
		t.Fatalf("replace state record: %v", err)
	}
	reopenedRecord, err := control.openRegularFileExact("file.state", false, linuxOutputStateFileMode)
	if err != nil {
		t.Fatalf("open replaced state record: %v", err)
	}
	same, err = linuxSameOpenRegularFile(newRecord, reopenedRecord)
	closeErr := reopenedRecord.close()
	if err != nil || closeErr != nil || !same {
		t.Fatalf("new state relation: same=%v compare=%v close=%v", same, err, closeErr)
	}

	// Retirement order is data-significant: the stage disappears first while
	// the anchor continues to witness the final object until the last step.
	if err := control.unlinkRegularFile("object.stage", stage); err != nil {
		t.Fatalf("unlink stage: %v", err)
	}
	same, err = linuxSameOpenRegularFile(anchor, final)
	if err != nil || !same {
		t.Fatalf("anchor after stage removal: same=%v err=%v", same, err)
	}
	if err := control.unlinkRegularFile("object.anchor", anchor); err != nil {
		t.Fatalf("unlink anchor: %v", err)
	}
	if err := control.unlinkRegularFile("file.state", newRecord); err != nil {
		t.Fatalf("unlink state record: %v", err)
	}
	if err := root.unlinkDirectory(".windshare-output", control); err != nil {
		t.Fatalf("unlink control directory: %v", err)
	}
}

func TestLinuxFailedFileCreateRollbackNeverDeletesReplacement(t *testing.T) {
	rootPath := t.TempDir()
	root, err := linuxOpenExt4OutputRoot(rootPath, &linuxHostOutputSystem)
	if err != nil {
		if errors.Is(err, errLinuxOutputUnsupported) {
			t.Skipf("test host is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatal(err)
	}
	defer root.close()

	system := linuxHostOutputSystem
	root.system = &system
	name := "rollback-race"
	path := filepath.Join(rootPath, name)
	displaced := filepath.Join(rootPath, "displaced-created-file")
	originalFsync := system.fsync
	injected := false
	system.fsync = func(fd int) error {
		if injected {
			return originalFsync(fd)
		}
		injected = true
		if err := os.Rename(path, displaced); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			return err
		}
		return unix.EIO
	}

	created, err := root.createRegularFileExact(name, linuxOutputStateFileMode, 1)
	if created != nil {
		_ = created.close()
	}
	if err == nil || !injected {
		t.Fatalf("create result=%v injected=%t, want injected failure", err, injected)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "replacement" {
		t.Fatalf("rollback mutated replacement: content=%q error=%v", got, readErr)
	}
}

func TestLinuxFailedDirectoryCreateWithoutPinRetainsReplacement(t *testing.T) {
	rootPath := t.TempDir()
	root, err := linuxOpenExt4OutputRoot(rootPath, &linuxHostOutputSystem)
	if err != nil {
		if errors.Is(err, errLinuxOutputUnsupported) {
			t.Skipf("test host is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatal(err)
	}
	defer root.close()

	system := linuxHostOutputSystem
	root.system = &system
	name := "rollback-directory-race"
	path := filepath.Join(rootPath, name)
	displaced := filepath.Join(rootPath, "displaced-created-directory")
	originalOpenat2 := system.openat2
	originalUnlinkat := system.unlinkat
	injected := false
	unlinks := 0
	system.openat2 = func(dirfd int, relative string, how *unix.OpenHow) (int, error) {
		if !injected && dirfd == root.fd && relative == name {
			injected = true
			if err := os.Rename(path, displaced); err != nil {
				return -1, err
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return -1, err
			}
			return -1, unix.EIO
		}
		return originalOpenat2(dirfd, relative, how)
	}
	system.unlinkat = func(dirfd int, relative string, flags int) error {
		if dirfd == root.fd && relative == name {
			unlinks++
		}
		return originalUnlinkat(dirfd, relative, flags)
	}

	created, err := root.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
	if created != nil {
		_ = created.close()
	}
	if err == nil || !injected {
		t.Fatalf("create result=%v injected=%t, want injected failure", err, injected)
	}
	if unlinks != 0 {
		t.Fatalf("rollback attempted %d name-only removals without a pinned object", unlinks)
	}
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		t.Fatalf("replacement directory was not retained: info=%v error=%v", info, statErr)
	}
}

func linuxCertificationTestSystem(
	filesystemMagic int64,
	mountType string,
	mountDeviceMajor uint32,
	returnUniqueMask bool,
) linuxOutputSystem {
	return linuxOutputSystem{
		getVersion:        func(int) (uint32, error) { return linuxTestGeneration, nil },
		getFlags:          func(int) (uint32, error) { return 0, nil },
		getFilesystemUUID: func(int) ([linuxFilesystemUUIDBytes]byte, error) { return linuxTestFilesystemUUID, nil },
		restartIdentity:   linuxStatxBirthTimeRestartIdentityProvider{},
		readProcessStatus: func() ([]byte, error) { return []byte("Umask:\t0022\n"), nil },
		statx: func(_ int, _ string, _ int, mask int, stat *unix.Statx_t) error {
			returnedMask := uint32(mask)
			mountID := uint64(linuxTestLegacyMountID)
			if mask&unix.STATX_MNT_ID_UNIQUE != 0 {
				mountID = linuxTestUniqueMountID
				if !returnUniqueMask {
					returnedMask &^= uint32(unix.STATX_MNT_ID_UNIQUE)
				}
			}
			*stat = unix.Statx_t{
				Mask:      returnedMask,
				Ino:       linuxTestRootInode,
				Mode:      unix.S_IFDIR | linuxOutputDirectoryMode,
				Dev_major: linuxTestDeviceMajor,
				Dev_minor: linuxTestDeviceMinor,
				Mnt_id:    mountID,
				Btime: unix.StatxTimestamp{
					Sec: linuxTestBirthSeconds, Nsec: linuxTestBirthNanos,
				},
			}
			return nil
		},
		fstatfs: func(_ int, stat *unix.Statfs_t) error {
			reflect.ValueOf(stat).Elem().FieldByName("Type").SetInt(filesystemMagic)
			stat.Fsid.Val = [2]int32{17, 29}
			return nil
		},
		readMountInfo: func() ([]byte, error) {
			line := fmt.Sprintf(
				"%d 1 %d:%d / /output rw - %s /dev/test rw\n",
				linuxTestLegacyMountID,
				mountDeviceMajor,
				linuxTestDeviceMinor,
				mountType,
			)
			return []byte(line), nil
		},
	}
}

func assertLinuxUnsupported(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("expected unsupported error, got %v", err)
	}
	var typed *linuxOutputUnsupportedError
	if !errors.As(err, &typed) {
		t.Fatalf("unsupported error is not typed: %T", err)
	}
}

func assertLinuxUnsafe(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("expected unsafe error, got %v", err)
	}
	var typed *linuxOutputUnsafeError
	if !errors.As(err, &typed) {
		t.Fatalf("unsafe error is not typed: %T", err)
	}
}
