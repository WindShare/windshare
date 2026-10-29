//go:build darwin

package outputdarwin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestPublicDirectoryUsesEffectiveCreateAuthority(t *testing.T) {
	for _, mode := range []uint32{0o775, 0o1777} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			path := t.TempDir()
			if err := unix.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			platform := openAuthorityFixture(t, path)
			checkSharedOutput(t, platform)

			// Existing shared descendants must use the same contract as the root.
			nestedPath := filepath.Join(path, "existing")
			if err := os.Mkdir(nestedPath, 0o775); err != nil {
				t.Fatal(err)
			}
			if err := unix.Chmod(nestedPath, mode); err != nil {
				t.Fatal(err)
			}
			nested, err := platform.root.OpenDirectory("existing", false)
			if err != nil {
				t.Fatal(err)
			}
			defer nested.Close()
			child, err := nested.CreateDirectory("child", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			file, err := nested.CreateFile("payload", false, 4)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNonOwnedPublicDirectoryAndMissingDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged receiver")
	}
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sudo", "-n", "chown", "0", parent).CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("macOS CI must exercise non-owned destinations: %v %s", err, out)
		}
		t.Skipf("non-owned fixture requires passwordless chown: %v %s", err, out)
	}
	// chmod after chown must also run as the fixture owner.
	if out, err := exec.Command("sudo", "-n", "chmod", "1777", parent).CombinedOutput(); err != nil {
		t.Fatalf("shared directory permissions: %v %s", err, out)
	}
	platform := openAuthorityFixture(t, parent)
	checkSharedOutput(t, platform)
	if err := platform.root.ValidateMetadataAuthority(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("non-owned container gained metadata authority: %v", err)
	}
	missing := filepath.Join(parent, "new-destination")
	created, err := Open(missing, true)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Close()
	if created.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatal("missing destination did not retain its creation disposition")
	}
	checkSharedOutput(t, created)
}

func TestPublicAuthorityRechecksKernelPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses discretionary access checks")
	}
	path := t.TempDir()
	t.Cleanup(func() { _ = unix.Chmod(path, privateDirectoryMode) })
	platform := openAuthorityFixture(t, path)
	for _, mode := range []uint32{0o555, 0o600} {
		if err := unix.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		guard, err := platform.AcquirePublicOperationGuard()
		if guard != nil {
			_ = guard.Close()
			t.Fatal("lost write/search authority acquired a guard")
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("mode=%o did not reject lost authority: %v", mode, err)
		}
	}
	if err := unix.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	setFixtureACL(t, path, "everyone deny add_file,add_subdirectory")
	if err := platform.root.ValidateCreateAuthority(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("ACL denial was ignored: %v", err)
	}
}

func TestACLGrantCanSupplyPublicCreateAuthority(t *testing.T) {
	path := t.TempDir()
	if err := unix.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	setFixtureACL(t, path, "everyone allow add_file,add_subdirectory,search,delete_child")
	platform := openAuthorityFixture(t, path)
	checkSharedOutput(t, platform)
}

func TestPrivateDirectoryIsolatesInheritedACLWithoutChangingPublicPolicy(t *testing.T) {
	path := t.TempDir()
	setFixtureACL(t, path, "everyone allow read,write,append,execute,file_inherit,directory_inherit")
	platform := openAuthorityFixture(t, path)
	before, err := readSecurity(platform.root.fd())
	if err != nil {
		t.Fatal(err)
	}
	private, err := platform.root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	control := private.(*directory)
	if err := control.verify(); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := control.CreateFile("checkpoint", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := readSecurity(platform.root.fd())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("public ACL changed: %v", err)
	}

	// An existing permissive directory is not a fresh object we can repair.
	existingPath := filepath.Join(path, "existing-control")
	if err := os.Mkdir(existingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := platform.root.OpenDirectory("existing-control", true)
	if existing != nil {
		_ = existing.Close()
		t.Fatal("inherited ACL admitted an existing private directory")
	}
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("existing private ACL was not rejected: %v", err)
	}
	ordinary, err := platform.root.OpenDirectory("existing-control", false)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if err := validatePrivateACL(ordinary.(*directory).fd()); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("opening existing state silently repaired its ACL: %v", err)
	}
}

func TestMetadataAuthorityDoesNotRequireChildCreation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses discretionary access checks")
	}
	path := t.TempDir()
	t.Cleanup(func() { _ = unix.Chmod(path, privateDirectoryMode) })
	platform := openAuthorityFixture(t, path)
	if err := unix.Chmod(path, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := platform.root.ValidateCreateAuthority(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only directory unexpectedly permits creation: %v", err)
	}
	modified, err := catalog.NewModifiedTime(1, 123456789, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.root.SetModifiedTime(modified); err != nil {
		t.Fatalf("owned metadata depends on child-creation permissions: %v", err)
	}
	if err := unix.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}
	setFixtureACL(t, path, "everyone deny writeattr")
	if err := platform.root.ValidateCreateAuthority(); err != nil {
		t.Fatalf("metadata denial disabled child creation: %v", err)
	}
	if err := platform.root.ValidateMetadataAuthority(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("metadata ACL denial was ignored: %v", err)
	}
}

func TestProfiledStageKeepsPublicDescriptorsAwayFromContent(t *testing.T) {
	path := t.TempDir()
	setFixtureACL(t, path, "everyone allow read,write,append,execute,file_inherit,directory_inherit")
	platform := openAuthorityFixture(t, path)
	template, err := platform.root.CreateFile("public-empty-template", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()
	templateState, _ := stateFrom(template)
	profile, err := readPublicFileProfile(templateState)
	if err != nil {
		t.Fatal(err)
	}
	private, err := platform.root.CreateDirectory("stage", true)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	stage, err := private.(*directory).createProfiledStage("partial", 4, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if same, err := stage.SameFile(template); err != nil || same {
		t.Fatalf("stage reuses an externally accessible inode: same=%t err=%v", same, err)
	}
	if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if size, err := template.Size(); err != nil || size != 0 {
		t.Fatalf("public descriptor observed download content: size=%d err=%v", size, err)
	}
	stageState, _ := stateFrom(stage)
	actual, err := readPublicFileProfile(stageState)
	if err != nil || actual.mode != profile.mode || actual.group != profile.group ||
		!bytes.Equal(actual.security, profile.security) {
		t.Fatalf("public permissions were not preserved: mode=%o/%o group=%d/%d err=%v",
			actual.mode, profile.mode, actual.group, profile.group, err)
	}
	outcome, err := platform.root.PublishFileNoReplace(stage, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish=%v err=%v", outcome, err)
	}
	final, err := platform.root.OpenObservedFile("final", false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if same, err := final.SameFile(stage); err != nil || !same {
		t.Fatalf("publication replaced the staged inode: same=%t err=%v", same, err)
	}
}

func TestSharedDirectoryCleanupRejectsObservedReplacement(t *testing.T) {
	path := t.TempDir()
	if err := unix.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	platform := openAuthorityFixture(t, path)
	owned, err := platform.root.CreateFile("entry", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if err := os.Rename(filepath.Join(path, "entry"), filepath.Join(path, "moved")); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(path, "entry")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := platform.root.RemoveFile("entry", owned); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("cleanup accepted a replacement: %v", err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
		t.Fatalf("cleanup changed the replacement: %q %v", data, err)
	}
}

func openAuthorityFixture(t *testing.T, path string) *Platform {
	t.Helper()
	platform, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := platform.Close(); err != nil {
			t.Error(err)
		}
	})
	return platform
}

func setFixtureACL(t *testing.T, path, acl string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", acl, path).CombinedOutput(); err != nil {
		t.Fatalf("set ACL: %v %s", err, out)
	}
	t.Cleanup(func() {
		// Some fixtures obtain write/search solely from this ACL.
		if err := unix.Chmod(path, privateDirectoryMode); err != nil {
			t.Error(err)
		}
		if out, err := exec.Command("/bin/chmod", "-N", path).CombinedOutput(); err != nil {
			t.Errorf("remove fixture ACL: %v %s", err, out)
		}
	})
}

func checkSharedOutput(t *testing.T, platform *Platform) {
	t.Helper()
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	capabilities, err := platform.DestinationCapabilities()
	if err != nil || !capabilities.SafePublish().Supported() {
		t.Fatalf("shared destination lost safe publication: %v", err)
	}
	private, err := platform.root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	stage, err := platform.root.CreateProcessStage(private, "partial", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := platform.root.PublishFileNoReplace(stage, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("shared publication=%v err=%v", outcome, err)
	}
	outcome, err = platform.root.PublishFileNoReplace(stage, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("shared publication overwrote a collision: %v %v", outcome, err)
	}
}
