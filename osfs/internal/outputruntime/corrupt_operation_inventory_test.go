package outputruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestCorruptOperationKeepsHealthyInventoryAndPreservesCorruptRecord(t *testing.T) {
	ctx := context.Background()
	root := newRuntimeTestRootSpec(t).path
	var fixtures []ordinaryResumeSessionFixture
	for _, seed := range []byte{0x81, 0x91} {
		fixture := openOrdinaryResumeSession(t, root, seed, 4)
		if err := fixture.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.transaction.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		pauseOrdinaryResumeFixture(t, fixture)
		fixtures = append(fixtures, fixture)
	}
	broken, healthy := fixtures[0], fixtures[1]
	recordPath := filepath.Join(root, checkpointstore.ControlDirectory,
		checkpointstore.OrdinaryRegistryDirectoryV1, "operations",
		bytesToHex(broken.intent.OperationID().Bytes()), "record")
	corruptBytes := []byte("invalid operation record")
	if err := os.WriteFile(recordPath, corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resumeauthority.New(repository)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := inventory.List(ctx)
	if err != nil || !listed.UnknownEntries() || len(listed.Summaries()) != 1 ||
		listed.Summaries()[0].OperationID() != healthy.intent.OperationID() {
		t.Fatalf("healthy operation disappeared: summaries=%v unknown=%t err=%v",
			listed.Summaries(), listed.UnknownEntries(), err)
	}
	if actual, err := os.ReadFile(recordPath); err != nil || !bytes.Equal(actual, corruptBytes) {
		t.Fatalf("inventory changed the corrupt record: %v", err)
	}
}
