package outputportable

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/recoverywitness"
)

func newWitnessFile(t *testing.T) (*directory, *file, []byte) {
	t.Helper()
	platform, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	directory := platform.Root().(*directory)
	created, err := directory.CreateFile("partial", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	file := created.(*file)
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	encoded, err := file.CreateRecoveryWitness()
	if err != nil {
		t.Fatal(err)
	}
	return directory, file, encoded
}

func TestPartialWitnessSurvivesNativeIdentityChange(t *testing.T) {
	directory, original, encoded := newWitnessFile(t)
	originalInfo, err := original.handle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(filepath.Join(directory.path, "partial"))
	if err != nil || len(physical) != 4+recoverywitness.TrailerSize {
		t.Fatalf("private envelope: %d %v", len(physical), err)
	}
	// Recreate the stored object with a different inode/file index, modeling
	// persistent bytes reopening under different mount-derived identities.
	if err := os.WriteFile(filepath.Join(directory.path, "reopened"), physical, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := directory.openFile("reopened", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedInfo, err := reopened.handle.Stat()
	if err != nil || os.SameFile(originalInfo, reopenedInfo) {
		t.Fatal("test did not change native identity")
	}
	if matched, err := reopened.BindRecoveryWitness(encoded); err != nil || !matched {
		t.Fatalf("recovery rejected retained bytes: %v %v", matched, err)
	}
	if size, err := reopened.Size(); err != nil || size != 4 {
		t.Fatalf("logical size: %d %v", size, err)
	}
	if n, err := reopened.ReadAt(make([]byte, 8), 0); n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("trailer escaped through payload read: %d %v", n, err)
	}
	if _, err := reopened.WriteAt([]byte("x"), 4); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatal("payload overwrote token")
	}
	if matched, err := reopened.RecoveryRemovalAllowed(encoded); err != nil || !matched {
		t.Fatalf("remount lost cleanup authority: %v %v", matched, err)
	}
	physical[len(physical)-1] ^= 1
	if _, err := reopened.handle.WriteAt(physical, 0); err != nil {
		t.Fatal(err)
	}
	if matched, err := reopened.BindRecoveryWitness(encoded); err != nil || matched {
		t.Fatal("replacement inherited ownership")
	}
	if matched, err := reopened.RecoveryRemovalAllowed(encoded); err != nil || matched {
		t.Fatal("replacement became removable")
	}
}

func TestPublicationEvidenceCannotAuthorizeTrailerlessDeletion(t *testing.T) {
	directory, original, partial := newWitnessFile(t)
	prepared, err := original.PrepareRecoveryPublication(partial)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := original.MatchesPublishedRecoveryWitness(prepared); err != nil || matched {
		t.Fatal("private trailer counted as published data")
	}
	if err := original.StripRecoveryWitness(prepared); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := directory.openFile("partial", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if matched, err := reopened.BindRecoveryWitness(prepared); err != nil || !matched {
		t.Fatalf("interrupted strip cannot resume: %v %v", matched, err)
	}
	if allowed, err := reopened.RecoveryRemovalAllowed(prepared); err != nil || allowed {
		t.Fatal("samples granted deletion authority")
	}
	if err := reopened.RestoreRecoveryWitness(); err != nil {
		t.Fatal(err)
	}
	if physical, _ := reopened.physicalSize(); physical != 4 {
		t.Fatal("reopen manufactured an ownership token")
	}
	if outcome, err := directory.PublishFileNoReplace(reopened, "final"); err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publication: %v %v", outcome, err)
	}
	final, err := directory.openFile("final", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if matched, err := final.MatchesPublishedRecoveryWitness(prepared); err != nil || !matched {
		t.Fatalf("published object unrecognized: %v %v", matched, err)
	}
	if matched, err := final.BindRecoveryWitness(partial); err != nil || matched {
		t.Fatal("unfinished witness authorized a public file")
	}
	if _, err := final.WriteAt([]byte("user"), 0); err != nil {
		t.Fatal(err)
	}
	if matched, err := final.MatchesPublishedRecoveryWitness(prepared); err != nil || matched {
		t.Fatal("replacement reconciled as delivered")
	}
}

func TestDefinitePublicationFailureCanRestoreLiveOwnership(t *testing.T) {
	_, file, partial := newWitnessFile(t)
	prepared, err := file.PrepareRecoveryPublication(partial)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.StripRecoveryWitness(prepared); err != nil {
		t.Fatal(err)
	}
	if err := file.RestoreRecoveryWitness(); err != nil {
		t.Fatal(err)
	}
	if allowed, err := file.RecoveryRemovalAllowed(prepared); err != nil || !allowed {
		t.Fatalf("known failure stranded owned data: %v %v", allowed, err)
	}
}

func TestDirectoryCreationEvidenceSurvivesSourceMtime(t *testing.T) {
	directory, _, _ := newWitnessFile(t)
	before, err := directory.PersistentDirectoryIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(946684800, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	after, err := directory.PersistentDirectoryIdentityClaim()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("directory mtime revoked recovery evidence")
	}
}
