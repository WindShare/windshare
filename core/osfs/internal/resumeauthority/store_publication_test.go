package resumeauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
)

func TestPinnedCheckpointComparesPublicFileOnlyWithExactOwnedAnchor(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x91)
	inventory, leased, snapshot := observeResumeFixture(t, fixture)
	provider, ok := leased.(PinnedCheckpointProvider)
	if !ok {
		t.Fatal("leased repository does not expose the opaque checkpoint seam")
	}
	checkpoint, ok := provider.PinnedCheckpoint(snapshot.Checkpoints()[0].RecordID())
	if !ok || checkpoint == nil || checkpoint.Record().Checksum() != fixture.record.Checksum() {
		t.Fatal("checkpoint pin did not retain the observed canonical record")
	}
	anchor, err := fixture.anchorShard.OpenFile(fixture.anchorName, true, false)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := checkpoint.SameOwnedFile(context.Background(), anchor)
	if err != nil || evidence != EvidenceExact {
		t.Fatalf("owned anchor comparison = %v, %v", evidence, err)
	}

	replacement := &memoryFile{data: &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}}
	evidence, err = checkpoint.SameOwnedFile(context.Background(), replacement)
	if err != nil || evidence != EvidenceReplaced {
		t.Fatalf("replacement comparison = %v, %v", evidence, err)
	}
	if err := errors.Join(anchor.Close(), replacement.Close(), leased.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.SameOwnedFile(context.Background(), replacement); err == nil {
		t.Fatal("closed lease retained publication-comparison authority")
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedCheckpointRejectsReplacedCanonicalRecordBeforeAnchorComparison(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x92)
	inventory, leased, snapshot := observeResumeFixture(t, fixture)
	provider := leased.(PinnedCheckpointProvider)
	checkpoint, ok := provider.PinnedCheckpoint(snapshot.Checkpoints()[0].RecordID())
	if !ok {
		t.Fatal("missing checkpoint pin")
	}
	anchor, err := fixture.anchorShard.OpenFile(fixture.anchorName, true, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.recordShard.mu.Lock()
	fixture.recordShard.files[fixture.recordName] = &memoryFileData{bytes: []byte("foreign")}
	fixture.recordShard.mu.Unlock()
	evidence, err := checkpoint.SameOwnedFile(context.Background(), anchor)
	if err != nil || evidence != EvidenceAmbiguous {
		t.Fatalf("replaced record comparison = %v, %v", evidence, err)
	}
	if err := errors.Join(anchor.Close(), leased.Close(), inventory.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeRepositoryClassifiesOwnershipWithoutCreatingState(t *testing.T) {
	root := newMemoryDirectory()
	config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x93)
	nativeConfig := NativeResumeConfig{
		Root: root, BackendID: config.Ownership.Backend(),
		Certification: config.Ownership.Certification(),
		RootIdentity:  config.Ownership.RootIdentity().Bytes(),
	}
	repository, err := NewNativeResumeRepository(nativeConfig)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Entries()) != 0 {
		t.Fatalf("absent namespace entries = %+v", inventory.Entries())
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	root.mu.Lock()
	created := len(root.dirs) != 0 || len(root.files) != 0
	root.mu.Unlock()
	if created {
		t.Fatal("read-only native discovery created checkpoint state")
	}

	control, err := openOrCreateDirectory(root, checkpointstore.ControlDirectory)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRoot, err := openOrCreateDirectory(control, checkpointstore.CheckpointDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(checkpointRoot.Close(), control.Close()); err != nil {
		t.Fatal(err)
	}
	inventory, err = repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publicInventory, err := NewInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	summaries := publicInventory.Summaries()
	if len(summaries) != 1 || !summaries[0].NeedsAttention() ||
		summaries[0].Attention()[0].Reason() != AttentionMissingOwnership {
		t.Fatalf("missing ownership summaries = %+v", summaries)
	}
	if err := publicInventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeRepositoryRecoversExactPersistedOwnershipAndClonesIdentity(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x94)
	rootIdentity := fixture.config.Ownership.RootIdentity().Bytes()
	repository, err := NewNativeResumeRepository(NativeResumeConfig{
		Root: fixture.root, BackendID: fixture.config.Ownership.Backend(),
		Certification: fixture.config.Ownership.Certification(), RootIdentity: rootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity[0] ^= 0xff
	inventory, err := repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publicInventory, err := NewInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	summaries := publicInventory.Summaries()
	if len(summaries) != 1 || summaries[0].NeedsAttention() {
		t.Fatalf("exact ownership summaries = %+v", summaries)
	}
	if err := publicInventory.Close(); err != nil {
		t.Fatal(err)
	}

	corruptOwnershipMarker(fixture.root)
	inventory, err = repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publicInventory, err = NewInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	summaries = publicInventory.Summaries()
	if len(summaries) != 1 || !summaries[0].NeedsAttention() ||
		summaries[0].Attention()[0].Reason() != AttentionCorruptBinding {
		t.Fatalf("corrupt ownership summaries = %+v", summaries)
	}
	if err := publicInventory.Close(); err != nil {
		t.Fatal(err)
	}
}
