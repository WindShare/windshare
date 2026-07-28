//go:build linux

package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

const linuxExt4NativeCertificationProfile = "linux-ext4"

func TestLinuxExt4NativeCertification(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	platform := openCertifiedNativeOutputForTest(
		t,
		t.TempDir(),
		linuxExt4NativeCertificationProfile,
		resumestate.CertificationLinuxExt4ProcessRestart,
	)
	if err := platform.Close(); err != nil {
		t.Fatalf("close Linux/ext4 certification authority: %v", err)
	}
}

func TestLinuxExt4ProcessRestartRecovery(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	runNativeOutputProcessRestartRecoveryTest(
		t,
		linuxExt4NativeCertificationProfile,
		resumestate.CertificationLinuxExt4ProcessRestart,
		outputlinux.ProbeNamePrefix+"31415926535897932384626433832795",
		0,
	)
	runNativeOutputSessionProcessRestartRecoveryTest(
		t,
		linuxExt4NativeCertificationProfile,
		resumestate.CertificationLinuxExt4ProcessRestart,
	)
}

func requireUnprivilegedLinuxExt4Certification(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == linuxExt4NativeCertificationProfile {
		t.Fatal("required Linux/ext4 certification must run as an ordinary unprivileged receiver")
	}
	t.Skip("Linux/ext4 native certification is meaningful only as an unprivileged receiver")
}

func TestLinuxExt4CreatesMissingRootThroughCertifiedHandles(t *testing.T) {
	target := filepath.Join(t.TempDir(), "first", "second")
	platform, err := openOutputV3Platform(target, true)
	if err != nil {
		nativeOutputCertificationFailure(t, linuxExt4NativeCertificationProfile,
			"create missing Linux/ext4 root through certified handles", err)
		return
	}
	defer func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close handle-created Linux/ext4 root: %v", err)
		}
	}()
	if binding, err := platform.RootBinding(); err != nil || binding.IsZero() {
		t.Fatalf("bind handle-created Linux/ext4 root: zero=%t err=%v", binding.IsZero(), err)
	}
}

func TestLinuxRootCreationDoesNotTraverseSymlinkAncestor(t *testing.T) {
	carrier := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(carrier, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skipf("create Linux symlink adversary: %v", err)
	}
	unexpected := filepath.Join(outside, "must-not-exist")
	platform, err := openOutputV3Platform(filepath.Join(alias, "must-not-exist"), true)
	if platform != nil {
		_ = platform.Close()
	}
	if err == nil {
		t.Fatal("root creation traversed a symlink ancestor")
	}
	if _, statErr := os.Lstat(unexpected); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("symlink target was mutated before certification: %v", statErr)
	}
}

func TestLinuxExt4DirectoryClaimsAndPinnedRemovalAreHandleBound(t *testing.T) {
	requireUnprivilegedLinuxExt4Certification(t)
	rootPath := t.TempDir()
	platform := openCertifiedNativeOutputForTest(
		t,
		rootPath,
		linuxExt4NativeCertificationProfile,
		resumestate.CertificationLinuxExt4ProcessRestart,
	)
	defer platform.Close()
	root := platform.Root()
	claim, err := root.IdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := root.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	repeated, err := duplicate.IdentityClaim()
	if err != nil || !claim.Equal(repeated) {
		t.Fatalf("duplicate directory claim differs: equal=%t error=%v", claim.Equal(repeated), err)
	}

	regularPath := filepath.Join(rootPath, "regular")
	if err := os.WriteFile(regularPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind, exact, err := root.ClassifyExactEntry("regular"); err != nil || kind != outputcap.EntryRegularFile || !exact {
		t.Fatalf("regular classification kind=%v exact=%t error=%v", kind, exact, err)
	}
	regular, err := root.OpenEntry("regular")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if regular.Kind() != outputcap.EntryRegularFile {
		t.Fatalf("pinned regular kind=%v", regular.Kind())
	}
	if err := root.RemoveEntry("regular", regular); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(regularPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned regular file still exists: %v", err)
	}
	racePath := filepath.Join(rootPath, "replacement-race")
	displacedPath := filepath.Join(rootPath, "displaced-original")
	if err := os.WriteFile(racePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	race, err := root.OpenEntry("replacement-race")
	if err != nil {
		t.Fatal(err)
	}
	defer race.Close()
	if err := os.Rename(racePath, displacedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(racePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEntry("replacement-race", race); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("pinned removal accepted a replacement object: %v", err)
	}
	if got, err := os.ReadFile(racePath); err != nil || string(got) != "replacement" {
		t.Fatalf("identity rejection mutated replacement: content=%q error=%v", got, err)
	}

	directoryPath := filepath.Join(rootPath, "empty-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	directoryEntry, err := root.OpenEntry("empty-directory")
	if err != nil {
		t.Fatal(err)
	}
	defer directoryEntry.Close()
	if directoryEntry.Kind() != outputcap.EntryDirectory {
		t.Fatalf("pinned directory kind=%v", directoryEntry.Kind())
	}
	openedDirectory, err := root.OpenPinnedDirectory(directoryEntry, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := openedDirectory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveEntry("empty-directory", directoryEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directoryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned directory still exists: %v", err)
	}

	targetPath := filepath.Join(rootPath, "target")
	if err := os.WriteFile(targetPath, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(rootPath, "opaque-link")
	if err := os.Symlink("target", linkPath); err != nil {
		t.Fatal(err)
	}
	if kind, exact, err := root.ClassifyExactEntry("opaque-link"); err != nil || kind != outputcap.EntryOther || !exact {
		t.Fatalf("opaque classification kind=%v exact=%t error=%v", kind, exact, err)
	}
	opaque, err := root.OpenEntry("opaque-link")
	if err != nil {
		t.Fatal(err)
	}
	defer opaque.Close()
	if opaque.Kind() != outputcap.EntryOther {
		t.Fatalf("pinned symbolic-link kind=%v", opaque.Kind())
	}
	if err := root.RemoveEntry("opaque-link", opaque); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(linkPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opaque symbolic link still exists: %v", err)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != "target" {
		t.Fatalf("opaque removal followed or mutated its target: content=%q error=%v", got, err)
	}
}
