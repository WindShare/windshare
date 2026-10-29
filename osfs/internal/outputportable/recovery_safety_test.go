package outputportable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestTaskLockExcludesAnotherOpenAndReleasesForRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "created")
	first, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	held, created, err := first.Root().AcquireLock("task.lock", false)
	if err != nil || !created {
		t.Fatalf("initial lock: %v %v", created, err)
	}
	if other, _, err := second.Root().AcquireLock("task.lock", true); !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("concurrent task admitted: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, created, err := second.Root().AcquireLock("task.lock", true)
	if err != nil || created {
		t.Fatalf("restart lock: %v %v", created, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if capabilities, err := first.(*Platform).DestinationCapabilities(); err != nil || !capabilities.RangeRecovery().Supported() || !capabilities.OperationRecovery().Supported() {
		t.Fatalf("ordinary files cannot retain progress: %v", err)
	}
	if first.(*Platform).IdentityScheme() != outputcap.IdentitySchemePortableWitnessV1 || first.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatal("portable identity was not admitted")
	}
	guard, err := first.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if same, err := guard.Root().SameDirectory(first.Root()); err != nil || !same {
		t.Fatal("operation guard changed destination")
	}
}

func TestRecoveryMetadataIgnoresMtimeAndCleanupRejectsReplacement(t *testing.T) {
	path := t.TempDir()
	p, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := p.Root().(*directory)
	original, err := d.CreateFile("partial", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if _, err := original.WriteAt([]byte("part"), 0); err != nil {
		t.Fatal(err)
	}
	if err := original.Sync(); err != nil {
		t.Fatal(err)
	}
	stamp, err := original.(*file).CreateRecoveryWitness()
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1700000000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	matches, err := original.(*file).BindRecoveryWitness(stamp)
	if err != nil || !matches {
		t.Fatal("mtime revoked recovery identity")
	}
	observed, err := d.OpenObservedFile("partial", true)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := original.SameFile(observed); err != nil || !same {
		t.Fatal("ordinary reopen changed identity")
	}
	if matched, err := observed.(*file).BindRecoveryWitness(stamp); err != nil || !matched {
		t.Fatalf("reopened partial lost its token: %v %v", matched, err)
	}
	if size, err := observed.Size(); err != nil || size != 4 {
		t.Fatalf("size %d %v", size, err)
	}
	data := make([]byte, 4)
	if _, err := observed.ReadAt(data, 0); err != nil || string(data) != "part" {
		t.Fatalf("data %q %v", data, err)
	}
	observed.Close()
	if err := os.Rename(filepath.Join(path, "partial"), filepath.Join(path, "saved")); err != nil {
		t.Fatal(err)
	}
	replacement, err := d.CreateFile("partial", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := d.RemoveFile("partial", original); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replacement deletion allowed: %v", err)
	}
	if err := d.RemoveFile("partial", replacement); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryEvidenceRejectsRebindingAndEscapedPaths(t *testing.T) {
	path := t.TempDir()
	p, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := p.Root().(*directory)
	for _, name := range []string{"", "..", "a/b", "../outside"} {
		if _, err := d.CreateFile(name, false, 0); err == nil {
			t.Fatalf("escaped component %q admitted", name)
		}
	}
	child, err := d.CreateDirectory("nested", false)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	claim, err := child.(*directory).PersistentDirectoryIdentityClaim()
	if err != nil || len(claim) == 0 {
		t.Fatal("missing metadata claim")
	}
	entry, err := d.OpenEntry("nested")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	if entry.Kind() != outputcap.EntryDirectory {
		t.Fatal("wrong entry type")
	}
	pinned, err := d.OpenPinnedDirectory(entry, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if same, err := pinned.SameDirectory(child); err != nil || !same {
		t.Fatal("pin changed directory")
	}
	if _, err := d.OpenMutableFile("nested", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("directory accepted as file: %v", err)
	}
	names, err := d.Names(1)
	if err != nil || len(names) != 1 {
		t.Fatalf("bounded inventory: %v %v", names, err)
	}
	if _, err := d.Names(0); err == nil {
		t.Fatal("inventory exceeded its bound")
	}
	if err := d.RemoveEntry("nested", entry); err != nil {
		t.Fatal(err)
	}
	replacement, err := d.CreateDirectory("nested", false)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if same, err := d.EntryMatches("nested", entry); err != nil || same {
		t.Fatalf("replacement inherited old entry claim: %v %v", same, err)
	}
	if err := d.RemoveDirectory("nested", child); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replacement directory deleted: %v", err)
	}
	if err := d.RemoveDirectory("nested", replacement); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicCheckpointReplacementKeepsCompleteReadableImage(t *testing.T) {
	p, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := p.Root().(*directory)
	initial, err := d.CreateFile("record", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.WriteAt([]byte("old"), 0); err != nil {
		t.Fatal(err)
	}
	initial.Close()
	candidate, err := d.CreateFile("candidate", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if _, err := candidate.WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplacePrivateFile(candidate, "record"); err != nil {
		t.Fatal(err)
	}
	current, err := d.OpenRecoveryDurabilityFile("record", true)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	data := make([]byte, 3)
	if _, err := current.(*file).ReadAt(data, 0); err != nil || string(data) != "new" {
		t.Fatalf("torn checkpoint: %q %v", data, err)
	}
	if kind, _, err := d.ClassifyExactEntry("candidate"); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("candidate not consumed: %v %v", kind, err)
	}
}
