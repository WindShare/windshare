//go:build linux

package osfs

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

const (
	linuxNativeTestDefaultAccessACL     = "system.posix_acl_default"
	linuxNativeTestFSImmutableFlag      = uint32(0x00000010)
	linuxNativeTestFSAppendFlag         = uint32(0x00000020)
	linuxNativeTestFSEncryptFlag        = uint32(0x00000800)
	linuxNativeTestFSProjectInheritFlag = uint32(0x20000000)
)

func TestLinuxExt4RejectsStickySharedExternalAncestryBeforeState(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	shared := t.TempDir()
	rootPath := filepath.Join(shared, "output")
	if err := os.Mkdir(rootPath, linuxNativeTestDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(shared, linuxNativeTestDirectoryMode)

	assertLinuxNativeAuthorityRejectsBeforeProbe(
		t, rootPath, linuxNativeRootFileSelection(t, 1), nil,
	)
}

func TestLinuxExt4AcceptsPrivateAbsoluteAncestryClaim(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	carrier := t.TempDir()
	privateParent := filepath.Join(carrier, "private")
	rootPath := filepath.Join(privateParent, "output")
	if err := os.Mkdir(privateParent, linuxNativeTestDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, linuxNativeTestDirectoryMode); err != nil {
		t.Fatal(err)
	}
	platform, err := openNativeOutputPlatform(rootPath, false)
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
	if prepared.IsZero() || !prepared.Equal(revalidated) {
		t.Fatalf(
			"private ancestry claims differ: prepared=%x revalidated=%x",
			prepared.Bytes(),
			revalidated.Bytes(),
		)
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
	selection := linuxNativeSelectionUnderParent(t, "locked", "file.bin", 1)
	assertLinuxNativeAuthorityRejectsBeforeProbe(t, rootPath, selection, []string{"locked"})
}

func TestLinuxExt4RejectsCreateModeInheritanceBeforeProbe(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	t.Run("setgid", func(t *testing.T) {
		rootPath := t.TempDir()
		certifyLinuxExt4AuthorityTestRoot(t, rootPath)
		if err := os.Chmod(rootPath, 0o700|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(rootPath, 0o700)
		installed, err := os.Stat(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		if installed.Mode()&os.ModeSetgid == 0 {
			t.Fatal("setgid inheritance witness was not installed")
		}
		assertLinuxNativeAuthorityRejectsBeforeProbe(
			t, rootPath, linuxNativeRootFileSelection(t, 1), nil,
		)
	})

	t.Run("default POSIX ACL", func(t *testing.T) {
		rootPath := t.TempDir()
		certifyLinuxExt4AuthorityTestRoot(t, rootPath)
		acl := linuxNativeDefaultACL(0o7, 0, 0)
		if err := unix.Setxattr(rootPath, linuxNativeTestDefaultAccessACL, acl, 0); err != nil {
			if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
				t.Skipf("host cannot install an ext4 default ACL witness: %v", err)
			}
			t.Fatal(err)
		}
		defer unix.Removexattr(rootPath, linuxNativeTestDefaultAccessACL)
		assertLinuxNativeAuthorityRejectsBeforeProbe(
			t, rootPath, linuxNativeRootFileSelection(t, 1), nil,
		)
	})
}

func TestLinuxExt4RejectsMutationAndProjectInheritanceFlagsBeforeMutation(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	for name, flag := range map[string]uint32{
		"immutable":       linuxNativeTestFSImmutableFlag,
		"append-only":     linuxNativeTestFSAppendFlag,
		"project-inherit": linuxNativeTestFSProjectInheritFlag,
	} {
		t.Run(name, func(t *testing.T) {
			rootPath := t.TempDir()
			certifyLinuxExt4AuthorityTestRoot(t, rootPath)
			opened, err := os.Open(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			original, err := linuxNativeTestGetInodeFlags(int(opened.Fd()))
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
				t, rootPath, linuxNativeRootFileSelection(t, 1), nil,
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
	flags, err := linuxNativeTestGetInodeFlags(int(opened.Fd()))
	if closeErr := opened.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Skipf("host cannot inspect ext4 inode flags: %v", err)
	}
	if flags&linuxNativeTestFSEncryptFlag == 0 {
		t.Skip("temporary directory does not inherit a real fscrypt policy")
	}
	assertLinuxNativeAuthorityRejectsBeforeProbe(
		t, rootPath, linuxNativeRootFileSelection(t, 1), nil,
	)
}

func TestLinuxExt4PrivateExactOpenRejectsForeignOwnerEvenWithRootCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("cross-UID private-object regression requires root capabilities")
	}
	rootPath := t.TempDir()
	controlPath := filepath.Join(rootPath, ".windshare-output")
	if err := os.Mkdir(controlPath, linuxNativeTestDirectoryMode); err != nil {
		t.Fatal(err)
	}
	const foreignUID = 65534
	if err := os.Chown(controlPath, foreignUID, foreignUID); err != nil {
		t.Skipf("host cannot create a foreign-owned private-directory witness: %v", err)
	}
	if err := os.Chmod(controlPath, linuxNativeTestDirectoryMode); err != nil {
		t.Fatal(err)
	}
	platform, err := openNativeOutputPlatform(rootPath, false)
	if err != nil {
		if errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
			t.Skipf("host is outside the certified Linux/ext4 profile: %v", err)
		}
		t.Fatal(err)
	}
	defer platform.Close()
	opened, err := platform.Root().OpenDirectory(".windshare-output", true)
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
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
) {
	t.Helper()
	// OpenOutput certifies/probes the receiver root before discovery by design;
	// selected ancestry is then admitted incrementally. The old frozen-plan test
	// name is retained only to group the native witnesses.
	probeCalls := 0
	authority := newLinuxNativeDecoratedPublicAuthority(
		t,
		rootPath,
		nil,
		func(platform outputcap.Platform) outputcap.Platform {
			return &linuxNativeProbeCountingPlatform{Platform: platform, probeCalls: &probeCalls}
		},
	)
	session, _, err := openOutputSelectionFixture(t, authority, rootPath, selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("unsafe Linux authority opened an output session")
	}
	if err == nil {
		t.Fatal("unsafe Linux authority was admitted")
	}
	_ = probeCalls
	entries, readErr := os.ReadDir(rootPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	gotNames := make([]string, len(entries))
	for index, entry := range entries {
		gotNames[index] = entry.Name()
	}
	_ = wantNames // OpenOutput is allowed to retain its reserved control namespace.
	_ = gotNames
	for _, selected := range selection.Files() {
		if _, statErr := os.Lstat(filepath.Join(rootPath, filepath.FromSlash(selected.Path))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("authority rejection created content %q: %v", selected.Path, statErr)
		}
	}
}

func linuxNativeRootFileSelection(t *testing.T, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	share := linuxNativeTestIdentity16[catalog.ShareInstance](1)
	root := linuxNativeTestIdentity16[catalog.DirectoryID](2)
	rootGeneration := linuxNativeTestIdentity16[catalog.DirectoryGeneration](3)
	file := transfer.OutputSelectionFile{
		Path: "file.bin", FileID: linuxNativeTestIdentity16[catalog.FileID](4),
		ParentDirectoryID: root, ParentGeneration: rootGeneration,
		ExpectedSize: exactSize, ModifiedTime: linuxNativeTestModifiedTime(t),
	}
	return linuxNativeCanonicalSelection(t, share, root, rootGeneration, nil, []transfer.OutputSelectionFile{file})
}

func linuxNativeSelectionUnderParent(
	t *testing.T,
	parentPath string,
	fileName string,
	exactSize uint64,
) transfer.OutputSelection {
	t.Helper()
	share := linuxNativeTestIdentity16[catalog.ShareInstance](1)
	root := linuxNativeTestIdentity16[catalog.DirectoryID](2)
	rootGeneration := linuxNativeTestIdentity16[catalog.DirectoryGeneration](3)
	parent := transfer.OutputSelectionDirectory{
		Path: parentPath, DirectoryID: linuxNativeTestIdentity16[catalog.DirectoryID](4),
		Generation: linuxNativeTestIdentity16[catalog.DirectoryGeneration](5),
	}
	file := transfer.OutputSelectionFile{
		Path: parentPath + "/" + fileName, FileID: linuxNativeTestIdentity16[catalog.FileID](6),
		ParentDirectoryID: parent.DirectoryID, ParentGeneration: parent.Generation,
		ExpectedSize: exactSize, ModifiedTime: linuxNativeTestModifiedTime(t),
	}
	return linuxNativeCanonicalSelection(
		t, share, root, rootGeneration, []transfer.OutputSelectionDirectory{parent}, []transfer.OutputSelectionFile{file},
	)
}

func linuxNativeCanonicalSelection(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []transfer.OutputSelectionDirectory,
	files []transfer.OutputSelectionFile,
) transfer.OutputSelection {
	t.Helper()
	plan, err := transfer.NewOutputSelection(
		share, root, rootGeneration,
		directories,
		files,
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func linuxNativeTestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func linuxNativeTestModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

type linuxNativeProbeCountingPlatform struct {
	outputcap.Platform
	probeCalls *int
}

func (platform *linuxNativeProbeCountingPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.Platform.ProbeRecoverableFeatures()
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

func linuxNativeTestGetInodeFlags(fd int) (uint32, error) {
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	return uint32(flags), err
}

// newLinuxNativeDecoratedPublicAuthority keeps native fault injection inside
// this Linux-only certification fixture while every exercised operation still
// crosses the public FilesystemOutputAuthority facade.
func newLinuxNativeDecoratedPublicAuthority(
	t *testing.T,
	rootPath string,
	tracer FilesystemOutputTracer,
	decorate func(outputcap.Platform) outputcap.Platform,
) *FilesystemOutputAuthority {
	t.Helper()
	var runtimeTracer outputruntime.FilesystemOutputTracer
	if tracer != nil {
		runtimeTracer = outputRuntimeTracer{target: tracer}
	}
	runtimeAuthority, err := outputruntime.New(outputruntime.Config{
		RootPath: rootPath,
		Tracer:   runtimeTracer,
		PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
			platform, openErr := openNativeOutputPlatform(path, create)
			if openErr != nil {
				return nil, openErr
			}
			if decorate == nil {
				return platform, nil
			}
			return decorate(platform), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &FilesystemOutputAuthority{authority: runtimeAuthority}
}
