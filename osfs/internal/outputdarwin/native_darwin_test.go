//go:build darwin

package outputdarwin

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestAPFSIdentityPublicationAndPlacement(t *testing.T) {
	path := t.TempDir()
	platform, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(platform.root.fd(), &fs); err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem=%s fsid=%v restart=%t", unix.ByteSliceToString(fs.Fstypename[:]), fs.Fsid, platform.root.volume.restart)
	caps, err := platform.DestinationCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	mode, err := outputcap.SelectExecutionMode(caps)
	if err != nil || mode != outputcap.ExecutionResumable {
		t.Fatalf("APFS execution=%v: %v", mode, err)
	}
	before, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	stageDir, err := platform.root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDir.Close()
	file, err := platform.root.CreateProcessStage(stageDir, "partial", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
	var first unix.Stat_t
	state, _ := stateFrom(file)
	if err := unix.Fstat(int(state.file.Fd()), &first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "final"), []byte("foreign"), privateFileMode); err != nil {
		t.Fatal(err)
	}
	outcome, err := platform.root.PublishFileNoReplace(file, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("collision=%v %v", outcome, err)
	}
	if err := os.Remove(filepath.Join(path, "final")); err != nil {
		t.Fatal(err)
	}
	outcome, err = platform.root.PublishFileNoReplace(file, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish=%v %v", outcome, err)
	}
	final, err := platform.root.OpenObservedFile("final", false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if same, err := file.SameFile(final); err != nil || !same {
		t.Fatalf("same inode=%t %v", same, err)
	}
	var last unix.Stat_t
	finalState, _ := stateFrom(final)
	if err := unix.Fstat(int(finalState.file.Fd()), &last); err != nil {
		t.Fatal(err)
	}
	if first.Ino != last.Ino || first.Blocks != last.Blocks {
		t.Fatal("publication copied the file")
	}
	after, err := platform.RootBinding()
	if err != nil || before != after {
		t.Fatalf("directory mutation changed persistent identity: %v", err)
	}
	other, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := other.RootBinding()
	if err = errors.Join(err, other.Close()); err != nil || reopened != before {
		t.Fatalf("reopen identity: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(path, "final"))
	if err != nil || !bytes.Equal(data, []byte("data")) {
		t.Fatalf("content=%q %v", data, err)
	}

	// A live descriptor follows a moved directory; public placement must not.
	moved := path + "-moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(path, privateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if guard, err := platform.AcquirePublicOperationGuard(); err == nil {
		_ = guard.Close()
		t.Fatal("replacement root acquired public authority")
	}
}

func TestAPFSPrivateModeDoesNotOverrideACLGrants(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	private, err := platform.root.CreateDirectory("private", true)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	path := filepath.Join(root, "private")
	if out, err := exec.Command("/bin/chmod", "+a", "everyone allow read,write", path).CombinedOutput(); err != nil {
		t.Fatalf("set ACL: %v %s", err, out)
	}
	if err := private.(*directory).verify(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("private ACL grant admitted: %v", err)
	}
}

func TestAPFSRestoringMetadataKeepsObjectIdentity(t *testing.T) {
	platform, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	before, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	file, err := platform.root.CreateFile("payload", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	witness, err := platform.root.OpenObservedFile("payload", false)
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close()
	modified, err := catalog.NewModifiedTime(1, 123456789, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := stateFrom(file)
	var old, current unix.Stat_t
	if err := unix.Fstat(int(state.file.Fd()), &old); err != nil {
		t.Fatal(err)
	}
	if err := file.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(int(state.file.Fd()), &current); err != nil {
		t.Fatal(err)
	}
	t.Logf("restore mtime: inode=%d birth_before=%v birth_after=%v", old.Ino, old.Btim, current.Btim)
	if old.Ino != current.Ino || current.Mtim.Sec != modified.Seconds() || current.Mtim.Nsec != int64(modified.Nanoseconds()) {
		t.Fatalf("mtime restoration changed the object or lost precision: %+v", current)
	}
	if same, err := file.SameFile(witness); err != nil || !same {
		t.Fatalf("metadata restoration revoked a pinned file: same=%t err=%v", same, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("metadata restoration blocked data-before-checkpoint flush: %v", err)
	}
	if err := platform.root.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	after, err := platform.RootBinding()
	if err != nil || before != after {
		t.Fatalf("metadata restoration changed persistent directory identity: %v", err)
	}
}
