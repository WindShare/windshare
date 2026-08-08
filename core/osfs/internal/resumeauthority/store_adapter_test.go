package resumeauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestResumeAdapterAcquiresLeaseBeforeIntentEnumerationAndReportsContention(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x31)
	adapter := mustResumeRepository(t, fixture.config)
	firstInventory, err := adapter.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls := directoryNamesCalls(fixture.intentDirectory); calls != 0 {
		t.Fatalf("intent enumerated while listing: %d", calls)
	}
	first, err := firstInventory.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls := directoryNamesCalls(fixture.intentDirectory); calls != 0 {
		t.Fatalf("intent enumerated before lease-backed Observe: %d", calls)
	}

	secondInventory, err := adapter.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondInventory.Acquire(context.Background(), 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("contending acquire error = %v", err)
	}
	if calls := directoryNamesCalls(fixture.intentDirectory); calls != 0 {
		t.Fatalf("contender enumerated selected intent: %d", calls)
	}
	if err := secondInventory.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := first.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.NamespaceEvidence() != EvidenceExact ||
		directoryNamesCalls(fixture.intentDirectory) == 0 {
		t.Fatalf("leased observation = %v, names calls = %d",
			snapshot.NamespaceEvidence(), directoryNamesCalls(fixture.intentDirectory))
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstInventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAdapterEnforcesRemoveThenSyncOrdering(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x41)
	inventory, leased, snapshot := observeResumeFixture(t, fixture)
	plan := discardPlanForSnapshot(t, snapshot)
	actions := plan.Actions()
	if len(actions) != 6 || actions[0].Kind() != ActionRemoveStage ||
		actions[1].Kind() != ActionSyncStages {
		t.Fatalf("actions = %v", actionKinds(actions))
	}
	if _, err := leased.Apply(context.Background(), actions[1]); err == nil {
		t.Fatal("out-of-order sync unexpectedly succeeded")
	}
	if !memoryFileExists(fixture.stageShard, fixture.stageName) {
		t.Fatal("out-of-order action mutated the stage")
	}

	baselineSyncs := directorySyncCalls(fixture.stageShard)
	result, err := leased.Apply(context.Background(), actions[0])
	if err != nil || result.Status() != ApplyCompleted {
		t.Fatalf("stage removal = %v, %v", result.Status(), err)
	}
	if memoryFileExists(fixture.stageShard, fixture.stageName) {
		t.Fatal("stage survived its authorized removal")
	}
	if syncs := directorySyncCalls(fixture.stageShard); syncs != baselineSyncs {
		t.Fatalf("removal collapsed sync cut: got %d, want %d", syncs, baselineSyncs)
	}
	result, err = leased.Apply(context.Background(), actions[1])
	if err != nil || result.Status() != ApplyCompleted {
		t.Fatalf("stage sync = %v, %v", result.Status(), err)
	}
	if syncs := directorySyncCalls(fixture.stageShard); syncs != baselineSyncs+1 {
		t.Fatalf("stage sync count = %d, want %d", syncs, baselineSyncs+1)
	}
	applyActions(t, leased, actions[2:])
	assertResumeArtifactsAbsent(t, fixture)
	closeResumeFixture(t, inventory, leased)
}

func TestResumeAdapterConvergesFromEveryDiscardCrashCut(t *testing.T) {
	for cut := 1; cut <= 5; cut++ {
		t.Run(actionCutName(cut), func(t *testing.T) {
			fixture := newResumeAdapterFixture(t, byte(0x50+cut))
			inventory, leased, snapshot := observeResumeFixture(t, fixture)
			plan := discardPlanForSnapshot(t, snapshot)
			actions := plan.Actions()
			applyActions(t, leased, actions[:cut])
			closeResumeFixture(t, inventory, leased)

			reopenedInventory, reopened, reopenedSnapshot := observeResumeFixture(t, fixture)
			reopenedPlan := discardPlanForSnapshot(t, reopenedSnapshot)
			if reopenedPlan.ExpectedStatus() == DiscardNeedsAttention {
				t.Fatalf("crash cut became attention: %+v", reopenedPlan.Attention())
			}
			applyActions(t, reopened, reopenedPlan.Actions())
			assertResumeArtifactsAbsent(t, fixture)
			closeResumeFixture(t, reopenedInventory, reopened)
		})
	}
}

func TestResumeAdapterPreservesUnknownAndReplacedObjectsForAttention(t *testing.T) {
	t.Run("unknown intent child", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x61)
		fixture.intentDirectory.mu.Lock()
		fixture.intentDirectory.files["foreign.child"] = &memoryFileData{bytes: []byte{1}}
		fixture.intentDirectory.mu.Unlock()

		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		plan := discardPlanForSnapshot(t, snapshot)
		if plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
			t.Fatalf("unknown-child plan = %v, %v", plan.ExpectedStatus(), actionKinds(plan.Actions()))
		}
		assertResumeArtifactsPresent(t, fixture)
		closeResumeFixture(t, inventory, leased)
	})

	t.Run("same-name stage replacement", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x62)
		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		plan := discardPlanForSnapshot(t, snapshot)
		fixture.stageShard.mu.Lock()
		replacement := &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
		fixture.stageShard.files[fixture.stageName] = replacement
		fixture.stageShard.mu.Unlock()

		result, err := leased.Apply(context.Background(), plan.Actions()[0])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("replacement result = %v, %v", result.Status(), err)
		}
		fixture.stageShard.mu.Lock()
		retained := fixture.stageShard.files[fixture.stageName] == replacement
		fixture.stageShard.mu.Unlock()
		if !retained || !memoryFileExists(fixture.anchorShard, fixture.anchorName) ||
			!memoryFileExists(fixture.recordShard, fixture.recordName) {
			t.Fatal("replacement attention removed uncertain state")
		}
		closeResumeFixture(t, inventory, leased)
	})

	t.Run("replacement between removal and sync", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x63)
		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		actions := discardPlanForSnapshot(t, snapshot).Actions()
		if _, err := leased.Apply(context.Background(), actions[0]); err != nil {
			t.Fatal(err)
		}
		fixture.stageShard.mu.Lock()
		replacement := &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
		fixture.stageShard.files[fixture.stageName] = replacement
		fixture.stageShard.mu.Unlock()

		result, err := leased.Apply(context.Background(), actions[1])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("sync-cut replacement = %v, %v", result.Status(), err)
		}
		if !memoryFileExists(fixture.anchorShard, fixture.anchorName) ||
			!memoryFileExists(fixture.recordShard, fixture.recordName) {
			t.Fatal("sync-cut replacement allowed later reductions")
		}
		closeResumeFixture(t, inventory, leased)
	})
}

func TestResumeAdapterRevalidatesCertifiedOwnershipAfterLease(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x71)
	adapter := mustResumeRepository(t, fixture.config)
	inventory, err := adapter.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	corruptOwnershipMarker(fixture.root)

	leased, err := inventory.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := leased.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := discardPlanForSnapshot(t, snapshot)
	if snapshot.NamespaceEvidence() == EvidenceExact ||
		plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
		t.Fatalf("ownership replacement = %v, plan %v",
			snapshot.NamespaceEvidence(), plan.ExpectedStatus())
	}
	if calls := directoryNamesCalls(fixture.intentDirectory); calls != 0 {
		t.Fatalf("corrupt ownership allowed intent enumeration: %d", calls)
	}
	assertResumeArtifactsPresent(t, fixture)
	closeResumeFixture(t, inventory, leased)
}

func TestResumeAdapterListsAbsentAndForeignNamespacesWithoutGrantingAuthority(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(
			t, root, checkpointmodel.CallerProvidedContainer, 0x79,
		)
		adapter := mustResumeRepository(t, config)
		inventory, err := adapter.ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if entries := inventory.Entries(); len(entries) != 0 {
			t.Fatalf("absent inventory entries = %d", len(entries))
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("foreign certified binding", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x7a)
		foreignOwnership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
			Backend:             fixture.config.Ownership.Backend(),
			Certification:       fixture.config.Ownership.Certification(),
			RootIdentity:        bytes.Repeat([]byte{0xee}, sha256.Size),
			RootOpenDisposition: fixture.config.Ownership.RootOpenDisposition(),
		})
		if err != nil {
			t.Fatal(err)
		}
		adapter := mustResumeRepository(t, checkpointstore.CertifiedConfig{
			Root: fixture.root, Ownership: foreignOwnership,
		})
		inventory, err := adapter.ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if entries := inventory.Entries(); len(entries) != 1 {
			t.Fatalf("foreign inventory entries = %d", len(entries))
		}
		if _, err := inventory.Acquire(context.Background(), 0); err == nil {
			t.Fatal("foreign inventory granted a lease capability")
		}
		assertResumeArtifactsPresent(t, fixture)
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("foreign certification and disposition", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x7d)
		foreignOwnership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
			Backend:             fixture.config.Ownership.Backend(),
			Certification:       checkpointmodel.CertificationLinuxExt4ProcessRestart,
			RootIdentity:        fixture.config.Ownership.RootIdentity().Bytes(),
			RootOpenDisposition: checkpointmodel.AuthorityCreatedRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		adapter := mustResumeRepository(t, checkpointstore.CertifiedConfig{
			Root: fixture.root, Ownership: foreignOwnership,
		})
		inventory, err := adapter.ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if entries := inventory.Entries(); len(entries) != 1 {
			t.Fatalf("foreign certification entries = %d", len(entries))
		}
		if _, err := inventory.Acquire(context.Background(), 0); err == nil {
			t.Fatal("foreign certification granted a lease capability")
		}
		assertResumeArtifactsPresent(t, fixture)
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestResumeAdapterTreatsCandidatesAndArtifactDivergenceAsIntentAtomicAttention(t *testing.T) {
	t.Run("deterministic candidate", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x7b)
		encoded, err := checkpointmodel.EncodeRecord(fixture.record)
		if err != nil {
			t.Fatal(err)
		}
		candidateName := checkpointstore.TemporaryName(fixture.recordName, encoded, 0)
		fixture.recordShard.mu.Lock()
		fixture.recordShard.files[candidateName] = &memoryFileData{bytes: encoded}
		fixture.recordShard.mu.Unlock()

		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		plan := discardPlanForSnapshot(t, snapshot)
		if plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
			t.Fatalf("candidate plan = %v, %v", plan.ExpectedStatus(), actionKinds(plan.Actions()))
		}
		if !memoryFileExists(fixture.recordShard, candidateName) {
			t.Fatal("ambiguous candidate was reconciled by read-only observation")
		}
		assertResumeArtifactsPresent(t, fixture)
		closeResumeFixture(t, inventory, leased)
	})

	t.Run("stage anchor divergence", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x7c)
		fixture.anchorShard.mu.Lock()
		fixture.anchorShard.files[fixture.anchorName] = &memoryFileData{
			bytes: make([]byte, fixture.record.ExactSize()),
		}
		fixture.anchorShard.mu.Unlock()

		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		plan := discardPlanForSnapshot(t, snapshot)
		if plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
			t.Fatalf("divergent artifact plan = %v, %v", plan.ExpectedStatus(), actionKinds(plan.Actions()))
		}
		assertResumeArtifactsPresent(t, fixture)
		closeResumeFixture(t, inventory, leased)
	})

	t.Run("decodable intent alias", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x7e)
		checkpointRoot := fixture.root.dirs[checkpointstore.ControlDirectory].dirs[checkpointstore.CheckpointDirectory]
		intents := checkpointRoot.dirs[checkpointstore.IntentsDirectory]
		canonicalName, _ := onlyMemoryDirectory(t, intents)
		intents.mu.Lock()
		intents.dirs[strings.ToUpper(canonicalName)] = newMemoryDirectory()
		intents.mu.Unlock()

		adapter := mustResumeRepository(t, fixture.config)
		inventoryPort, err := adapter.ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory := inventoryPort.(*resumeInventory)
		canonicalIndex := -1
		for index, item := range inventory.items {
			if item.name == canonicalName {
				canonicalIndex = index
				break
			}
		}
		if canonicalIndex < 0 {
			t.Fatal("canonical intent was not listed")
		}
		leased, err := inventory.Acquire(context.Background(), canonicalIndex)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := leased.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plan := discardPlanForSnapshot(t, snapshot)
		if plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
			t.Fatalf("alias plan = %v, %v", plan.ExpectedStatus(), actionKinds(plan.Actions()))
		}
		assertResumeArtifactsPresent(t, fixture)
		closeResumeFixture(t, inventory, leased)
	})
}

type resumeAdapterFixture struct {
	root            *memoryDirectory
	config          checkpointstore.CertifiedConfig
	intentDirectory *memoryDirectory
	record          checkpointmodel.Record
	recordShard     *memoryDirectory
	recordName      string
	stageShard      *memoryDirectory
	stageName       string
	anchorShard     *memoryDirectory
	anchorName      string
}

func newResumeAdapterFixture(t *testing.T, fill byte) resumeAdapterFixture {
	t.Helper()
	root := newMemoryDirectory()
	config, intent := certifiedFixture(
		t, root, checkpointmodel.CallerProvidedContainer, fill,
	)
	namespace, err := checkpointstore.Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(err)
	}
	candidate := checkpointRecordFixture(t, config.Ownership, intent, fill+2)
	record, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(record); err != nil {
		t.Fatal(err)
	}
	checkpointRoot := root.dirsForTest(t, checkpointstore.ControlDirectory).
		dirsForTest(t, checkpointstore.CheckpointDirectory)
	intents := checkpointRoot.dirsForTest(t, checkpointstore.IntentsDirectory)
	_, intentDirectory := onlyMemoryDirectory(t, intents)
	records := intentDirectory.dirsForTest(t, checkpointstore.RecordsDirectory)
	stages := intentDirectory.dirsForTest(t, checkpointstore.StagesDirectory)
	anchors := intentDirectory.dirsForTest(t, checkpointstore.AnchorsDirectory)
	recordShardName, recordName := checkpointstore.RecordLocation(record.RecordID())
	recordShard := records.dirsForTest(t, recordShardName)
	stageShardName, stageName := recoveryArtifactLocation(t, record.OwnedOutputObject(), checkpointstore.RecoveryStage)
	anchorShardName, anchorName := recoveryArtifactLocation(t, record.OwnedOutputObject(), checkpointstore.RecoveryAnchor)
	stageShard := mustMemoryShard(t, stages, stageShardName)
	anchorShard := mustMemoryShard(t, anchors, anchorShardName)
	stage, err := stageShard.CreateFile(stageName, true, int64(record.ExactSize()))
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := anchorShard.LinkFileNoReplace(stage, anchorName)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(stage.Close(), anchor.Close(), stageShard.Sync(), anchorShard.Sync()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(repository.Close(), lease.Close(), namespace.Close()); err != nil {
		t.Fatal(err)
	}
	intentDirectory.mu.Lock()
	intentDirectory.namesCalls = 0
	intentDirectory.mu.Unlock()
	return resumeAdapterFixture{
		root: root, config: config, intentDirectory: intentDirectory, record: record,
		recordShard: recordShard, recordName: recordName,
		stageShard: stageShard, stageName: stageName,
		anchorShard: anchorShard, anchorName: anchorName,
	}
}

func mustResumeRepository(t *testing.T, config checkpointstore.CertifiedConfig) ResumeRepository {
	t.Helper()
	repository, err := NewResumeRepository(config)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func observeResumeFixture(
	t *testing.T,
	fixture resumeAdapterFixture,
) (PinnedInventory, LeasedRepository, RepositorySnapshot) {
	t.Helper()
	adapter := mustResumeRepository(t, fixture.config)
	inventory, err := adapter.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries := inventory.Entries()
	if len(entries) != 1 {
		t.Fatalf("inventory entries = %d", len(entries))
	}
	leased, err := inventory.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := leased.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return inventory, leased, snapshot
}

func discardPlanForSnapshot(
	t *testing.T,
	snapshot RepositorySnapshot,
) DiscardPlan {
	t.Helper()
	publications := make([]PublicationObservation, 0, len(snapshot.Checkpoints()))
	for _, checkpoint := range snapshot.Checkpoints() {
		publication, err := NewPublicationObservation(
			checkpoint.RecordID(), EvidenceAbsent,
		)
		if err != nil {
			t.Fatal(err)
		}
		publications = append(publications, publication)
	}
	plan := ReduceDiscard(snapshot, publications)
	if !plan.Valid() {
		t.Fatalf("invalid discard plan: %+v", plan)
	}
	return plan
}

func applyActions(
	t *testing.T,
	leased LeasedRepository,
	actions []Action,
) {
	t.Helper()
	for _, action := range actions {
		result, err := leased.Apply(context.Background(), action)
		if err != nil {
			t.Fatalf("apply %v: %v", action.Kind(), err)
		}
		if result.Status() == ApplyNeedsAttention {
			t.Fatalf("apply %v needs attention: %+v", action.Kind(), result.Attention())
		}
	}
}

func closeResumeFixture(
	t *testing.T,
	inventory PinnedInventory,
	leased LeasedRepository,
) {
	t.Helper()
	if err := leased.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustMemoryShard(
	t *testing.T,
	parent outputcap.Directory,
	name string,
) *memoryDirectory {
	t.Helper()
	shard, err := openOrCreateDirectory(parent, name)
	if err != nil {
		t.Fatal(err)
	}
	return shard.(*memoryDirectory)
}

func (directory *memoryDirectory) dirsForTest(t *testing.T, name string) *memoryDirectory {
	t.Helper()
	directory.mu.Lock()
	defer directory.mu.Unlock()
	child := directory.dirs[name]
	if child == nil {
		t.Fatalf("missing directory %q", name)
	}
	return child
}

func directoryNamesCalls(directory *memoryDirectory) int {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.namesCalls
}

func directorySyncCalls(directory *memoryDirectory) int {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.syncCalls
}

func memoryFileExists(directory *memoryDirectory, name string) bool {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	_, exists := directory.files[name]
	return exists
}

func assertResumeArtifactsPresent(t *testing.T, fixture resumeAdapterFixture) {
	t.Helper()
	if !memoryFileExists(fixture.stageShard, fixture.stageName) ||
		!memoryFileExists(fixture.anchorShard, fixture.anchorName) ||
		!memoryFileExists(fixture.recordShard, fixture.recordName) {
		t.Fatal("expected recovery artifacts to remain")
	}
}

func assertResumeArtifactsAbsent(t *testing.T, fixture resumeAdapterFixture) {
	t.Helper()
	if memoryFileExists(fixture.stageShard, fixture.stageName) ||
		memoryFileExists(fixture.anchorShard, fixture.anchorName) ||
		memoryFileExists(fixture.recordShard, fixture.recordName) {
		t.Fatal("recovery artifacts survived discard")
	}
}

func corruptOwnershipMarker(root *memoryDirectory) {
	control := root.dirs[checkpointstore.ControlDirectory]
	checkpointRoot := control.dirs[checkpointstore.CheckpointDirectory]
	checkpointRoot.mu.Lock()
	checkpointRoot.files[checkpointstore.OwnershipFile] = &memoryFileData{bytes: []byte("foreign")}
	checkpointRoot.mu.Unlock()
}

func actionKinds(actions []Action) []ActionKind {
	kinds := make([]ActionKind, len(actions))
	for index, action := range actions {
		kinds[index] = action.Kind()
	}
	return kinds
}

func actionCutName(cut int) string {
	return []string{"", "stage-remove", "stage-sync", "anchor-remove", "anchor-sync", "record-remove"}[cut]
}

type memoryEntryReference struct {
	kind      outputcap.EntryKind
	directory *memoryDirectory
	file      *memoryFileData
}

func (reference *memoryEntryReference) Kind() outputcap.EntryKind { return reference.kind }

func (*memoryEntryReference) Close() error { return nil }

func (directory *memoryDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if child := directory.dirs[name]; child != nil {
		return &memoryEntryReference{kind: outputcap.EntryDirectory, directory: child}, nil
	}
	if data := directory.files[name]; data != nil {
		return &memoryEntryReference{kind: outputcap.EntryRegularFile, file: data}, nil
	}
	return nil, fs.ErrNotExist
}

func (directory *memoryDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	reference, ok := expected.(*memoryEntryReference)
	if !ok || reference == nil {
		return false, outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	switch reference.kind {
	case outputcap.EntryDirectory:
		return directory.dirs[name] == reference.directory, nil
	case outputcap.EntryRegularFile:
		return directory.files[name] == reference.file, nil
	default:
		return false, nil
	}
}

func (*memoryDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	_ bool,
) (outputcap.Directory, error) {
	reference, ok := expected.(*memoryEntryReference)
	if !ok || reference == nil || reference.kind != outputcap.EntryDirectory ||
		reference.directory == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return reference.directory, nil
}

func (directory *memoryDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	reference, ok := expected.(*memoryEntryReference)
	if !ok || reference == nil {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if reference.kind != outputcap.EntryRegularFile {
		return outputcap.ErrUnsafeNamespace
	}
	data := directory.files[name]
	if data == nil {
		return fs.ErrNotExist
	}
	if data != reference.file {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.files, name)
	return nil
}

func (file *memoryFile) SameFile(other outputcap.File) (bool, error) {
	peer, ok := other.(*memoryFile)
	return ok && peer != nil && file.data == peer.data, nil
}
