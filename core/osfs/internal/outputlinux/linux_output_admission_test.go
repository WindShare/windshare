//go:build linux

package outputlinux

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

func TestLinuxRuntimeAdmissionDoesNotRequireRestartIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*linuxOutputSystem)
	}{
		{"missing UUID provider", func(system *linuxOutputSystem) { system.getFilesystemUUID = nil }},
		{"unsupported UUID", func(system *linuxOutputSystem) {
			system.getFilesystemUUID = func(int) ([linuxFilesystemUUIDBytes]byte, error) {
				return [linuxFilesystemUUIDBytes]byte{}, unix.ENOTTY
			}
		}},
		{"missing restart provider", func(system *linuxOutputSystem) { system.restartIdentity = nil }},
		{"missing birth time", func(system *linuxOutputSystem) {
			original := system.statx
			system.statx = func(fd int, path string, flags, mask int, stat *unix.Statx_t) error {
				if err := original(fd, path, flags, mask, stat); err != nil {
					return err
				}
				stat.Mask &^= unix.STATX_BTIME
				return nil
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true)
			system.openat2 = func(int, string, *unix.OpenHow) (int, error) { return 10, nil }
			system.close = func(int) error { return nil }
			system.faccessat2 = func(int, string, uint32, int) error { return nil }
			test.mutate(&system)
			root, err := linuxOpenOutputRoot("/output", &system)
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			if root.binding.restart != nil || root.binding.mount.filesystemUUID != [linuxFilesystemUUIDBytes]byte{} {
				t.Fatal("runtime-only admission retained a partial restart certificate")
			}
			platform := &linuxV3Platform{root: &linuxV3Directory{native: root}}
			assertLinuxProcessOnlyPlatform(t, platform)
			if err := root.verifyHandle(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLinuxRuntimeAdmissionPreservesCertificationAndRejectsContradictions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		configure       func(*linuxPlacementTestHarness, *linuxOutputSystem)
		wantCertificate bool
		wantUnsafe      bool
	}{
		{name: "certified", wantCertificate: true},
		{name: "ancestor lacks restart identity", configure: func(harness *linuxPlacementTestHarness, system *linuxOutputSystem) {
			original := system.getFilesystemUUID
			system.getFilesystemUUID = func(fd int) ([linuxFilesystemUUIDBytes]byte, error) {
				if fd == harness.root.fd {
					return [linuxFilesystemUUIDBytes]byte{}, unix.ENOTTY
				}
				return original(fd)
			}
		}},
		{name: "ancestry changed", wantUnsafe: true, configure: func(harness *linuxPlacementTestHarness, _ *linuxOutputSystem) {
			harness.replaceAfterOpen = true
		}},
		{name: "access denied", wantUnsafe: true, configure: func(_ *linuxPlacementTestHarness, system *linuxOutputSystem) {
			system.faccessat2 = func(int, string, uint32, int) error { return unix.EACCES }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			harness, _ := newLinuxPlacementTestHarness()
			system := harness.system()
			original := system.openat2
			system.openat2 = func(fd int, path string, how *unix.OpenHow) (int, error) {
				if fd == unix.AT_FDCWD && path == "/home/receiver/output" {
					return harness.root.children["home"].children["receiver"].children["output"].fd, nil
				}
				return original(fd, path, how)
			}
			if test.configure != nil {
				test.configure(harness, &system)
			}
			root, err := linuxOpenOutputRoot("/home/receiver/output", &system)
			if test.wantUnsafe {
				if root != nil || !errors.Is(err, errLinuxOutputUnsafe) {
					t.Fatalf("contradiction admitted: root=%v error=%v", root, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer root.close()
			if (root.binding.restart != nil) != test.wantCertificate {
				t.Fatalf("certificate=%v expected=%v", root.binding.restart, test.wantCertificate)
			}
			if test.wantCertificate {
				platform := &linuxV3Platform{root: &linuxV3Directory{native: root}}
				if platform.Certification() != outputcap.CertificationLinuxExt4ProcessRestart {
					t.Fatalf("lost exact ext4 certificate: %v", platform.Certification())
				}
				if _, err := platform.RootBinding(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestLinuxRuntimeAdmissionRejectsUnfixedNamespaces(t *testing.T) {
	t.Parallel()
	for _, filesystem := range []string{
		"nfs", "nfs4", "cifs", "smb3", "9p", "ceph", "afs", "coda",
		"virtiofs", "overlay", "ecryptfs", "fuse", "fuseblk", "fuse.sshfs",
	} {
		t.Run(filesystem, func(t *testing.T) {
			t.Parallel()
			system := linuxCertificationTestSystem(linuxExt4SuperMagic, filesystem, linuxTestDeviceMajor, true)
			if _, err := linuxBindOutputFD(&system, 10); !errors.Is(err, errLinuxOutputUnsupported) {
				t.Fatalf("admitted %s namespace: %v", filesystem, err)
			}
		})
	}
}

func TestLinuxRuntimeBindingStillRejectsMountChangesAndUnknownDirectoryPolicies(t *testing.T) {
	t.Parallel()
	system := linuxCertificationTestSystem(unix.TMPFS_MAGIC, "tmpfs", linuxTestDeviceMajor, true)
	system.getFlags = func(int) (uint32, error) { return 0, unix.ENOTTY }
	binding, err := linuxBindOutputFD(&system, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := linuxVerifyOpenObject(&system, 10, binding); err != nil {
		t.Fatal(err)
	}
	binding.filesystem.magic = linuxExt4SuperMagic
	if _, err := linuxVerifyOpenObject(&system, 10, binding); !errors.Is(err, errLinuxOutputUnsafe) {
		t.Fatalf("filesystem replacement accepted: %v", err)
	}
	system = linuxCertificationTestSystem(linuxExt4SuperMagic, "ext4", linuxTestDeviceMajor, true)
	system.getFlags = func(int) (uint32, error) { return 0, unix.ENOTTY }
	if _, err := linuxBindOutputFD(&system, 10); !errors.Is(err, errLinuxOutputUnsupported) {
		t.Fatalf("unknown directory flags accepted: %v", err)
	}
}

func TestLinuxTmpfsSupportsProcessOutputWithoutClaimingRestartRecovery(t *testing.T) {
	t.Parallel()
	platform, rootPath := newLinuxTmpfsOutputPlatform(t)
	assertLinuxProcessOnlyPlatform(t, platform)

	// An old process's names do not grant ownership to this process, even when
	// their spelling resembles a probe namespace.
	leftoverName := linuxOutputProbePrefix + "11111111111111111111111111111111"
	leftover := filepath.Join(rootPath, leftoverName)
	if err := os.Mkdir(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "untouched"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	capabilities, err := platform.DestinationCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.SafePublish().Supported() || capabilities.OperationRecovery().Supported() ||
		capabilities.RangeRecovery().Supported() || capabilities.CrashCleanup().Supported() {
		t.Fatalf("incorrect tmpfs capabilities: %+v", capabilities)
	}
	if mode, err := outputcap.SelectExecutionMode(capabilities); err != nil || mode != outputcap.ExecutionLiveOnly {
		t.Fatalf("tmpfs execution mode=%v error=%v", mode, err)
	}
	if err := platform.ProbeRecoverableFeatures(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("tmpfs acquired restart support: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(leftover, "untouched")); err != nil || string(got) != "old" {
		t.Fatalf("prior-process artifact changed: %q %v", got, err)
	}

	root := platform.root
	stageDirectory, err := root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDirectory.Close()
	payload := []byte("out-of-order verified output")
	file, err := root.CreateProcessStage(stageDirectory, "stage", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	split := len(payload) / 2
	if _, err := file.WriteAt(payload[split:], int64(split)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(payload[:split], 0); err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_456_789, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	if same, err := file.MetadataMatches(uint64(len(payload)), modified); err != nil || !same {
		t.Fatalf("metadata mismatch=%v error=%v", !same, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "collision"), []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome, err := root.PublishFileNoReplace(file, "collision"); err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("collision=%v error=%v", outcome, err)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "collision")); err != nil || string(got) != "sentinel" {
		t.Fatalf("collision overwritten: %q %v", got, err)
	}
	published, err := root.LinkFileNoReplace(file, "result")
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if same, err := published.SameFile(file); err != nil || !same {
		t.Fatalf("publication copied or replaced the inode: %v %v", same, err)
	}
	if err := stageDirectory.RemoveFile("stage", file); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("private", stageDirectory); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "result")); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published content=%q error=%v", got, err)
	}
}

func TestLinuxTmpfsProcessStageRetainsWitnessAcrossInstallFailure(t *testing.T) {
	t.Parallel()
	platform, _ := newLinuxTmpfsOutputPlatform(t)
	root := platform.root
	stageDirectory, err := root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDirectory.Close()
	system := *root.native.system
	root.native.system = &system
	stageNative := stageDirectory.(*linuxV3Directory).native
	stageNative.system = &system
	originalSync := system.fsync
	system.fsync = func(fd int) error {
		if fd == stageNative.fd {
			return unix.EIO
		}
		return originalSync(fd)
	}
	file, err := root.CreateProcessStage(stageDirectory, "stage", 1)
	system.fsync = originalSync
	if file == nil || !errors.Is(err, unix.EIO) {
		t.Fatalf("post-install failure lost pinned witness: file=%T error=%v", file, err)
	}
	defer file.Close()
	if err := stageDirectory.RemoveFile("stage", file); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("private", stageDirectory); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxTmpfsProcessAuthorityRejectsSymlinksAndReplacementDeletion(t *testing.T) {
	t.Parallel()
	platform, rootPath := newLinuxTmpfsOutputPlatform(t)
	root := platform.root
	if err := os.Symlink("/tmp", filepath.Join(rootPath, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.OpenDirectory("escape", false); err == nil {
		t.Fatalf("symlink traversal accepted: %v", err)
	}
	file, err := root.CreateFile("owned", false, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(filepath.Join(rootPath, "owned"), filepath.Join(rootPath, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "owned"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveFile("owned", file); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replacement deletion accepted: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "owned")); err != nil || string(got) != "foreign" {
		t.Fatalf("replacement changed: %q %v", got, err)
	}
}

func TestLinuxTmpfsCreatesNestedOutputRoot(t *testing.T) {
	t.Parallel()
	_, rootPath := newLinuxTmpfsOutputPlatform(t)
	path := filepath.Join(rootPath, "nested", "output")
	platform, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if platform.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatalf("root disposition=%v", platform.RootOpenDisposition())
	}
	assertLinuxProcessOnlyPlatform(t, platform.(*linuxV3Platform))
	file, err := platform.Root().CreateFile("created", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := os.Stat(filepath.Join(path, "created")); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxTmpfsProcessStageInheritsFinalParentACL(t *testing.T) {
	t.Parallel()
	platform, rootPath := newLinuxTmpfsOutputPlatform(t)
	root := platform.root
	stageDirectory, err := root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDirectory.Close()
	linuxTestSetDefaultACL(t, rootPath, linuxTestDefaultACL(uint32(unix.Geteuid()+1), linuxTestACLRead))
	nestedValue, err := root.CreateDirectory("nested", false)
	if err != nil {
		t.Fatal(err)
	}
	defer nestedValue.Close()
	nested := nestedValue.(*linuxV3Directory)
	nestedPath := filepath.Join(rootPath, "nested")
	linuxTestSetDefaultACL(t, nestedPath, linuxTestDefaultACL(uint32(unix.Geteuid()+2), linuxTestACLWrite))
	file, err := nested.CreateProcessStage(stageDirectory, "stage", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reference, err := nested.CreateFile("reference", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	stageACL := linuxTestReadACL(t, filepath.Join(rootPath, "private", "stage"), linuxTestAccessACLXattr)
	parentACL := linuxTestReadACL(t, filepath.Join(nestedPath, "reference"), linuxTestAccessACLXattr)
	if !bytes.Equal(stageACL, parentACL) {
		t.Fatalf("stage lost final-parent ACL: stage=%x expected=%x", stageACL, parentACL)
	}
	if outcome, err := nested.PublishFileNoReplace(file, "final"); err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publication outcome=%v error=%v", outcome, err)
	}
	if finalACL := linuxTestReadACL(t, filepath.Join(nestedPath, "final"), linuxTestAccessACLXattr); !bytes.Equal(finalACL, parentACL) {
		t.Fatalf("publication changed final-parent ACL: final=%x expected=%x", finalACL, parentACL)
	}
}

func newLinuxTmpfsOutputPlatform(t *testing.T) (*linuxV3Platform, string) {
	t.Helper()
	const tmpfsRoot = "/dev/shm"
	var filesystem unix.Statfs_t
	if err := unix.Statfs(tmpfsRoot, &filesystem); err != nil || filesystem.Type != unix.TMPFS_MAGIC {
		t.Skipf("native tmpfs fixture unavailable: %v", err)
	}
	path, err := os.MkdirTemp(tmpfsRoot, "windshare-live-output-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	opened, err := Open(path, false)
	if err != nil {
		t.Fatalf("admit actual tmpfs output: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	})
	return opened.(*linuxV3Platform), path
}

func assertLinuxProcessOnlyPlatform(t *testing.T, platform *linuxV3Platform) {
	t.Helper()
	if platform.Certification() != "" || platform.Durability() != transfer.DurabilityNone ||
		platform.LiveCleanupNativeProfile() != 0 {
		t.Fatal("process-only platform advertised certified persistence")
	}
	if _, err := platform.RootBinding(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("persistent root identity available: %v", err)
	}
	if _, err := platform.root.PersistentDirectoryIdentityClaim(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("persistent directory identity available: %v", err)
	}
	if _, err := platform.root.PreparePersistentDirectoryIdentityClaim(); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("persistent directory enrollment available: %v", err)
	}
}
