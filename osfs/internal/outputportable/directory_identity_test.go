package outputportable

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDestinationIdentityConcurrentEnrollmentAndRestart(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	const contenders = 8
	type result struct {
		id  outputcap.DestinationBindingID
		err error
	}
	results := make(chan result, contenders)
	var workers sync.WaitGroup
	for range contenders {
		workers.Go(func() {
			id, err := platform.(*Platform).BindDestinationIdentity(platform.Root())
			results <- result{id, err}
		})
	}
	workers.Wait()
	close(results)
	var expected outputcap.DestinationBindingID
	for result := range results {
		if result.err != nil || result.id.IsZero() {
			t.Fatalf("enrollment: %v", result.err)
		}
		if !expected.IsZero() && result.id != expected {
			t.Fatal("concurrent bind split the destination identity")
		}
		expected = result.id
	}
	reopened, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	actual, err := reopened.(*Platform).BindDestinationIdentity(reopened.Root())
	if err != nil || actual != expected {
		t.Fatalf("restart identity: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("enrollment left candidates: %v %v", entries, err)
	}
	if err := reopened.(*Platform).RetireDestinationIdentity(reopened.Root(), expected); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("retired destination retained identity: %v %v", entries, err)
	}
}

func TestDestinationIdentityPreservesCorruptOrForeignState(t *testing.T) {
	platform, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	directory := platform.Root().(*directory)
	id, err := platform.(*Platform).BindDestinationIdentity(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory.path, destinationIdentityName)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 1
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.(*Platform).BindDestinationIdentity(directory); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("corrupt identity was re-enrolled: %v", err)
	}
	if err := platform.(*Platform).RetireDestinationIdentity(directory, id); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("corrupt identity was removed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, encoded) {
		t.Fatal("unrecognized identity was modified")
	}
}

func TestDirectoryIdentityRetirementPreservesPayloadAndModifiedTime(t *testing.T) {
	platform, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	directory := platform.Root().(*directory)
	claim, err := directory.CreateRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory.path, "payload"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTime := time.Unix(1700000000, 123456700)
	if err := setDirectoryTimes(directory, sourceTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(directory.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.RetireRecoveryIdentity(claim); err != nil {
		t.Fatal(err)
	}
	if err := directory.RetireRecoveryIdentity(claim); err != nil {
		t.Fatalf("retirement is not restartable: %v", err)
	}
	after, err := os.Stat(directory.path)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("private marker removal changed source mtime")
	}
	entries, err := os.ReadDir(directory.path)
	if err != nil || len(entries) != 1 || entries[0].Name() != "payload" {
		t.Fatalf("retirement changed delivered entries: %v %v", entries, err)
	}
}

func TestDirectoryIdentityReopenNeverEnrollsAReplacement(t *testing.T) {
	root := t.TempDir()
	original, err := Open(filepath.Join(root, "result"), true)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := original.Root().(*directory).CreateRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "result"), filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(filepath.Join(root, "result"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	directory := replacement.Root().(*directory)
	if matched, err := directory.MatchesRecoveryIdentity(claim); err != nil || matched {
		t.Fatalf("replacement matched retained witness: %t %v", matched, err)
	}
	entries, err := os.ReadDir(directory.path)
	if err != nil || len(entries) != 0 {
		t.Fatal("recovery created authority in an unrelated directory")
	}
	name, _ := directoryIdentityName(claim)
	if err := os.WriteFile(filepath.Join(directory.path, name), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := directory.RetireRecoveryIdentity(claim); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unrecognized marker removed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory.path, name))
	if err != nil || string(data) != "foreign" {
		t.Fatal("foreign file changed")
	}
}
