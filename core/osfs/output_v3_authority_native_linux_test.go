//go:build linux

package osfs

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

func TestLinuxExt4RejectsStickySharedExternalAncestryBeforeState(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	shared := t.TempDir()
	rootPath := filepath.Join(shared, "output")
	if err := os.Mkdir(rootPath, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(shared, linuxOutputDirectoryMode)

	assertLinuxNativeAuthorityRejectsBeforeProbe(
		t, rootPath, v3RecoverySelection(t, true, 1), nil,
	)
}

func TestLinuxExt4AcceptsPrivateAbsoluteAncestryClaim(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	carrier := t.TempDir()
	privateParent := filepath.Join(carrier, "private")
	rootPath := filepath.Join(privateParent, "output")
	if err := os.Mkdir(privateParent, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	prepared, err := platform.Root().PrepareIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	revalidated, err := platform.Root().IdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) == 0 || !reflect.DeepEqual(prepared, revalidated) {
		t.Fatalf("private ancestry claims differ: prepared=%x revalidated=%x", prepared, revalidated)
	}
}

func TestLinuxExt4RejectsNonWritableSelectedParentBeforeProbe(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	certifyLinuxExt4AuthorityTestRoot(t, rootPath)
	parentPath := filepath.Join(rootPath, "locked")
	if err := os.Mkdir(parentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parentPath, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parentPath, 0o755)
	selection := v3RecoverySelectionPaths(t, []string{"locked/file.bin"}, 1)
	assertLinuxNativeAuthorityRejectsBeforeProbe(t, rootPath, selection, []string{"locked"})
}

func TestLinuxExt4RejectsCreateModeInheritanceBeforeProbe(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	t.Run("setgid", func(t *testing.T) {
		rootPath := t.TempDir()
		certifyLinuxExt4AuthorityTestRoot(t, rootPath)
		if err := os.Chmod(rootPath, 0o2700); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(rootPath, 0o700)
		assertLinuxNativeAuthorityRejectsBeforeProbe(
			t, rootPath, v3RecoverySelection(t, true, 1), nil,
		)
	})

	t.Run("default POSIX ACL", func(t *testing.T) {
		rootPath := t.TempDir()
		certifyLinuxExt4AuthorityTestRoot(t, rootPath)
		acl := linuxNativeDefaultACL(0o7, 0, 0)
		if err := unix.Setxattr(rootPath, linuxDefaultAccessACL, acl, 0); err != nil {
			if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
				t.Skipf("host cannot install an ext4 default ACL witness: %v", err)
			}
			t.Fatal(err)
		}
		defer unix.Removexattr(rootPath, linuxDefaultAccessACL)
		assertLinuxNativeAuthorityRejectsBeforeProbe(
			t, rootPath, v3RecoverySelection(t, true, 1), nil,
		)
	})
}

func TestLinuxExt4RejectsMutationAndProjectInheritanceFlagsBeforeMutation(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	for name, flag := range map[string]uint32{
		"immutable":       linuxFSImmutableFlag,
		"append-only":     linuxFSAppendFlag,
		"project-inherit": linuxFSProjectInheritFlag,
	} {
		t.Run(name, func(t *testing.T) {
			rootPath := t.TempDir()
			certifyLinuxExt4AuthorityTestRoot(t, rootPath)
			opened, err := os.Open(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			original, err := linuxGetInodeFlags(int(opened.Fd()))
			if err != nil {
				t.Skipf("host cannot inspect ext4 inode flags: %v", err)
			}
			if err := unix.IoctlSetPointerInt(
				int(opened.Fd()), unix.FS_IOC_SETFLAGS, int(original|flag),
			); err != nil {
				if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EOPNOTSUPP) {
					t.Skipf("host cannot install ext4 %s witness: %v", name, err)
				}
				t.Fatal(err)
			}
			defer unix.IoctlSetPointerInt(int(opened.Fd()), unix.FS_IOC_SETFLAGS, int(original))
			assertLinuxNativeAuthorityRejectsBeforeProbe(
				t, rootPath, v3RecoverySelection(t, true, 1), nil,
			)
		})
	}
}

func TestLinuxExt4RejectsInheritedFscryptDirectoryBeforeProbe(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	opened, err := os.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := linuxGetInodeFlags(int(opened.Fd()))
	if closeErr := opened.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Skipf("host cannot inspect ext4 inode flags: %v", err)
	}
	if flags&linuxFSEncryptFlag == 0 {
		t.Skip("temporary directory does not inherit a real fscrypt policy")
	}
	assertLinuxNativeAuthorityRejectsBeforeProbe(
		t, rootPath, v3RecoverySelection(t, true, 1), nil,
	)
}

func TestLinuxExt4RejectsNestedMountIdentityBeforeProbe(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	certifyLinuxExt4AuthorityTestRoot(t, rootPath)
	if err := os.Mkdir(filepath.Join(rootPath, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	mismatchCalls := 0
	assertLinuxNativeAuthorityRejectsBeforeProbe(
		t,
		rootPath,
		v3RecoverySelectionPaths(t, []string{"nested/file.bin"}, 1),
		[]string{"nested"},
		func(platform outputV3Platform) {
			native, ok := platform.(*linuxV3Platform)
			if !ok || native.root == nil || native.root.native == nil || native.root.native.system == nil {
				t.Fatalf("native Linux platform = %#v", platform)
			}
			system := *native.root.native.system
			hostOpenat2 := system.openat2
			hostStatx := system.statx
			nestedFD := -1
			system.openat2 = func(dirfd int, path string, how *unix.OpenHow) (int, error) {
				fd, err := hostOpenat2(dirfd, path, how)
				if err == nil && path == "nested" {
					if how.Resolve&uint64(unix.RESOLVE_NO_XDEV) == 0 {
						t.Fatal("selected-parent open omitted RESOLVE_NO_XDEV")
					}
					nestedFD = fd
				}
				return fd, err
			}
			system.statx = func(fd int, path string, flags int, mask int, stat *unix.Statx_t) error {
				if err := hostStatx(fd, path, flags, mask, stat); err != nil {
					return err
				}
				if fd == nestedFD && path == "" && mask&unix.STATX_MNT_ID_UNIQUE != 0 {
					stat.Mnt_id = native.root.native.certificate.mount.uniqueMountID + 1
					mismatchCalls++
				}
				return nil
			}
			native.root.native.system = &system
		},
	)
	if mismatchCalls == 0 {
		t.Fatal("selected nested directory never reached the injected mount-identity check")
	}
}

func TestLinuxExt4PrivateExactOpenRejectsForeignOwnerEvenWithRootCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("cross-UID private-object regression requires root capabilities")
	}
	rootPath := t.TempDir()
	controlPath := filepath.Join(rootPath, ".windshare-output")
	if err := os.Mkdir(controlPath, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	const foreignUID = 65534
	if err := os.Chown(controlPath, foreignUID, foreignUID); err != nil {
		t.Skipf("host cannot create a foreign-owned private-directory witness: %v", err)
	}
	if err := os.Chmod(controlPath, linuxOutputDirectoryMode); err != nil {
		t.Fatal(err)
	}
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		if errors.Is(err, errUnsupportedOutputVolume) {
			t.Skipf("host is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatal(err)
	}
	defer platform.Close()
	opened, err := platform.Root().OpenDirectory(".windshare-output", true)
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, errOutputV3Unsafe) {
		t.Fatalf("foreign-owned exact private directory open = %v", err)
	}
}

func certifyLinuxExt4AuthorityTestRoot(t *testing.T, rootPath string) {
	t.Helper()
	platform := openCertifiedNativeOutputForTest(
		t, rootPath, linuxExt4NativeCertificationProfile,
		resumestate.CertificationLinuxExt4ProcessRestart,
	)
	if platform != nil {
		if err := platform.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertLinuxNativeAuthorityRejectsBeforeProbe(
	t *testing.T,
	rootPath string,
	selection transfer.OutputSelection,
	wantNames []string,
	configure ...func(outputV3Platform),
) {
	t.Helper()
	authority := v3RecoveryAuthority(t, rootPath, nil)
	nativeFactory := authority.platformFactory
	probeCalls := 0
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		if len(configure) > 0 {
			configure[0](platform)
		}
		return &linuxV3NativeProbeCountingPlatform{
			outputV3Platform: platform, probeCalls: &probeCalls,
		}, nil
	}
	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		if concrete, ok := session.(*filesystemOutputSession); ok {
			_ = concrete.closeHandles()
		}
		t.Fatal("unsafe Linux authority opened an output session")
	}
	if err == nil {
		t.Fatal("unsafe Linux authority was admitted")
	}
	if probeCalls != 0 {
		t.Fatalf("unsafe Linux authority reached the native probe %d times", probeCalls)
	}
	entries, readErr := os.ReadDir(rootPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	gotNames := make([]string, len(entries))
	for index, entry := range entries {
		gotNames[index] = entry.Name()
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("authority rejection root entries = %v, want %v (error %v)", gotNames, wantNames, err)
	}
	for _, selected := range selection.Files() {
		if _, statErr := os.Lstat(filepath.Join(rootPath, filepath.FromSlash(selected.Path))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("authority rejection created content %q: %v", selected.Path, statErr)
		}
	}
}

type linuxV3NativeProbeCountingPlatform struct {
	outputV3Platform
	probeCalls *int
}

func (platform *linuxV3NativeProbeCountingPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.outputV3Platform.ProbeRecoverableFeatures()
}

func linuxNativeDefaultACL(owner, group, other uint16) []byte {
	const (
		aclVersion  = uint32(2)
		aclUserObj  = uint16(0x01)
		aclGroupObj = uint16(0x04)
		aclOther    = uint16(0x20)
		undefinedID = ^uint32(0)
	)
	encoded := make([]byte, 4+3*8)
	binary.LittleEndian.PutUint32(encoded, aclVersion)
	for index, entry := range []struct {
		tag  uint16
		perm uint16
	}{
		{aclUserObj, owner}, {aclGroupObj, group}, {aclOther, other},
	} {
		offset := 4 + index*8
		binary.LittleEndian.PutUint16(encoded[offset:], entry.tag)
		binary.LittleEndian.PutUint16(encoded[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(encoded[offset+4:], undefinedID)
	}
	return encoded
}
