package resumeauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestResumeClosureInventoryDistinguishesEmptyOpaqueAndUnavailableState(t *testing.T) {
	t.Run("certified empty namespace", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x11)
		namespace, err := checkpointstore.Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := namespace.Close(); err != nil {
			t.Fatal(err)
		}

		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if entries := inventory.Entries(); len(entries) != 0 {
			t.Fatalf("certified empty entries = %d", len(entries))
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown root child without an intent", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x12)
		namespace, err := checkpointstore.Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := namespace.Close(); err != nil {
			t.Fatal(err)
		}
		checkpointRoot := resumeClosureCheckpointRoot(t, root)
		checkpointRoot.mu.Lock()
		checkpointRoot.files["future.state"] = &memoryFileData{bytes: []byte("opaque")}
		checkpointRoot.mu.Unlock()

		inventoryPort, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory := inventoryPort.(*resumeInventory)
		if len(inventory.items) != 1 {
			t.Fatalf("unknown-root inventory = %+v", inventory.items)
		}
		if _, err := inventory.Acquire(context.Background(), 0); err == nil {
			t.Fatal("opaque root state granted mutation authority")
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("opaque intent name", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x13)
		namespace, err := checkpointstore.Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := namespace.Close(); err != nil {
			t.Fatal(err)
		}
		intents := resumeClosureCheckpointRoot(t, root).dirsForTest(t, checkpointstore.IntentsDirectory)
		intents.mu.Lock()
		intents.dirs["foreign-intent"] = newMemoryDirectory()
		intents.mu.Unlock()

		inventoryPort, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory := inventoryPort.(*resumeInventory)
		if len(inventory.items) != 1 || !inventory.items[0].intent.IsZero() ||
			inventory.items[0].attention[0].Reason() != AttentionUnknownChildren {
			t.Fatalf("opaque intent item = %+v", inventory.items)
		}
		if _, err := inventory.Acquire(context.Background(), 0); err == nil {
			t.Fatal("opaque intent granted a lease")
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("canonical intent with the wrong kind", func(t *testing.T) {
		root := newMemoryDirectory()
		config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x14)
		namespace, err := checkpointstore.Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := namespace.Close(); err != nil {
			t.Fatal(err)
		}
		intents := resumeClosureCheckpointRoot(t, root).dirsForTest(t, checkpointstore.IntentsDirectory)
		intentName := intentNamespaceNameForTest(intent)
		intents.mu.Lock()
		intents.files[intentName] = &memoryFileData{bytes: []byte("not a directory")}
		intents.mu.Unlock()

		inventoryPort, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory := inventoryPort.(*resumeInventory)
		if len(inventory.items) != 1 || inventory.items[0].intent != intent ||
			inventory.items[0].attention[0].Reason() != AttentionCorruptBinding {
			t.Fatalf("wrong-kind intent item = %+v", inventory.items)
		}
		leased, err := inventory.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := leased.Observe(context.Background())
		if err != nil || snapshot.NamespaceEvidence() == EvidenceExact {
			t.Fatalf("wrong-kind intent snapshot = %v, %v", snapshot.NamespaceEvidence(), err)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("intent disappears while it is pinned", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x15)
		_, intents, _, _, _ := resumeClosureIntentLayout(t, fixture)
		tree := newResumeClosureTree()
		tree.behavior(intents).openEntry = func(name string) (outputcap.CurrentEntryReference, error) {
			return nil, fs.ErrNotExist
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)

		inventoryPort, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		inventory := inventoryPort.(*resumeInventory)
		if len(inventory.items) != 1 || inventory.items[0].pin != nil ||
			inventory.items[0].attention[0].Reason() != AttentionReplacement {
			t.Fatalf("disappeared intent item = %+v", inventory.items)
		}
		if _, err := inventory.Acquire(context.Background(), 0); err == nil {
			t.Fatal("disappeared intent retained acquisition authority")
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("list failure retains close failures", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x16)
		namespace, err := checkpointstore.Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := namespace.Close(); err != nil {
			t.Fatal(err)
		}
		intents := resumeClosureCheckpointRoot(t, root).dirsForTest(t, checkpointstore.IntentsDirectory)
		listErr := errors.New("intent inventory read failed")
		closeErr := errors.New("root pin close failed")
		tree := newResumeClosureTree()
		tree.behavior(intents).names = func(int) ([]string, error) { return nil, listErr }
		tree.behavior(root).closeErr = closeErr
		config.Root = tree.wrap(root)

		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if inventory != nil || !errors.Is(err, listErr) || !errors.Is(err, closeErr) {
			t.Fatalf("list/close aggregation = %T, %v", inventory, err)
		}
	})
}

func TestResumeClosureLeaseRevalidatesEveryListedNamespacePin(t *testing.T) {
	tests := []struct {
		name    string
		replace func(resumeAdapterFixture, resumeAdapterFixture)
	}{
		{
			name: "control directory",
			replace: func(current, replacement resumeAdapterFixture) {
				current.root.mu.Lock()
				current.root.dirs[checkpointstore.ControlDirectory] = replacement.root.dirsForTest(t, checkpointstore.ControlDirectory)
				current.root.mu.Unlock()
			},
		},
		{
			name: "checkpoint directory",
			replace: func(current, replacement resumeAdapterFixture) {
				control := current.root.dirsForTest(t, checkpointstore.ControlDirectory)
				control.mu.Lock()
				control.dirs[checkpointstore.CheckpointDirectory] = resumeClosureCheckpointRoot(t, replacement.root)
				control.mu.Unlock()
			},
		},
		{
			name: "ownership marker identity",
			replace: func(current, replacement resumeAdapterFixture) {
				checkpointRoot := resumeClosureCheckpointRoot(t, current.root)
				replacementRoot := resumeClosureCheckpointRoot(t, replacement.root)
				replacementRoot.mu.Lock()
				marker := replacementRoot.files[checkpointstore.OwnershipFile]
				replacementRoot.mu.Unlock()
				checkpointRoot.mu.Lock()
				checkpointRoot.files[checkpointstore.OwnershipFile] = marker
				checkpointRoot.mu.Unlock()
			},
		},
		{
			name: "intents directory",
			replace: func(current, replacement resumeAdapterFixture) {
				checkpointRoot := resumeClosureCheckpointRoot(t, current.root)
				checkpointRoot.mu.Lock()
				checkpointRoot.dirs[checkpointstore.IntentsDirectory] = resumeClosureCheckpointRoot(t, replacement.root).dirsForTest(t, checkpointstore.IntentsDirectory)
				checkpointRoot.mu.Unlock()
			},
		},
		{
			name: "leases directory",
			replace: func(current, replacement resumeAdapterFixture) {
				checkpointRoot := resumeClosureCheckpointRoot(t, current.root)
				checkpointRoot.mu.Lock()
				checkpointRoot.dirs[checkpointstore.LeasesDirectory] = resumeClosureCheckpointRoot(t, replacement.root).dirsForTest(t, checkpointstore.LeasesDirectory)
				checkpointRoot.mu.Unlock()
			},
		},
		{
			name: "selected intent directory",
			replace: func(current, replacement resumeAdapterFixture) {
				_, intents, _, _, _ := resumeClosureIntentLayout(t, current)
				_, replacementIntents, _, _, _ := resumeClosureIntentLayout(t, replacement)
				intentName, _ := onlyMemoryDirectory(t, intents)
				intents.mu.Lock()
				intents.dirs[intentName] = replacementIntents.dirsForTest(t, intentName)
				intents.mu.Unlock()
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fill := byte(0x20 + index)
			fixture := newResumeAdapterFixture(t, fill)
			replacement := newResumeAdapterFixture(t, fill)
			inventory, err := mustResumeRepository(t, fixture.config).ListResumeState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			test.replace(fixture, replacement)

			leased, err := inventory.Acquire(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := leased.Observe(context.Background())
			if err != nil || snapshot.NamespaceEvidence() != EvidenceReplaced ||
				len(snapshot.Checkpoints()) != 0 {
				t.Fatalf("replacement snapshot = %v checkpoints=%d err=%v",
					snapshot.NamespaceEvidence(), len(snapshot.Checkpoints()), err)
			}
			resumeClosureClose(t, inventory, leased)
		})
	}
}

func TestResumeClosureObservationPreservesMalformedAndDuplicateOwnership(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0x31)
	intentDirectory, _, records, stages, anchors := resumeClosureIntentLayout(t, fixture)

	duplicate := resumeClosureRecordWithObject(
		t, fixture.record, "folder/duplicate.bin", 0x91, fixture.record.OwnedOutputObject(),
	)
	encodedDuplicate, err := checkpointmodel.EncodeRecord(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	duplicateShardName, duplicateName := checkpointstore.RecordLocation(duplicate.RecordID())
	duplicateShard := mustMemoryShard(t, records, duplicateShardName)
	writeMemoryFile(t, duplicateShard, duplicateName, encodedDuplicate)

	malformedID := mustRecordID(t, 0xa1)
	malformedShardName, malformedName := checkpointstore.RecordLocation(malformedID)
	malformedShard := mustMemoryShard(t, records, malformedShardName)
	writeMemoryFile(t, malformedShard, malformedName, []byte("malformed committed record"))
	fixture.recordShard.mu.Lock()
	fixture.recordShard.files["opaque-record"] = &memoryFileData{bytes: []byte("unknown")}
	fixture.recordShard.mu.Unlock()
	records.mu.Lock()
	records.dirs["not-a-shard"] = newMemoryDirectory()
	records.mu.Unlock()

	orphanObject, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0xb1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	stageShardName, stageName := recoveryArtifactLocation(t, orphanObject, checkpointstore.RecoveryStage)
	orphanStageShard := mustMemoryShard(t, stages, stageShardName)
	orphanStageShard.mu.Lock()
	orphanStageShard.files[stageName] = &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
	orphanStageShard.files["opaque-stage"] = &memoryFileData{bytes: []byte("unknown")}
	orphanStageShard.mu.Unlock()
	anchorShardName, anchorName := recoveryArtifactLocation(t, orphanObject, checkpointstore.RecoveryAnchor)
	orphanAnchorShard := mustMemoryShard(t, anchors, anchorShardName)
	orphanAnchorShard.mu.Lock()
	orphanAnchorShard.files[anchorName] = &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
	orphanAnchorShard.mu.Unlock()

	wrongKindObject, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0xb2}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	wrongKindShardName, wrongKindName := recoveryArtifactLocation(t, wrongKindObject, checkpointstore.RecoveryStage)
	wrongKindShard := mustMemoryShard(t, stages, wrongKindShardName)
	wrongKindShard.mu.Lock()
	wrongKindShard.dirs[wrongKindName] = newMemoryDirectory()
	wrongKindShard.mu.Unlock()

	inventory, leased, snapshot := observeResumeFixture(t, fixture)
	if snapshot.NamespaceEvidence() != EvidenceExact || len(snapshot.Checkpoints()) != 2 {
		t.Fatalf("malformed inventory snapshot = %v checkpoints=%d",
			snapshot.NamespaceEvidence(), len(snapshot.Checkpoints()))
	}
	reasons := make(map[AttentionReason]bool)
	for _, attention := range snapshot.Attention() {
		reasons[attention.Reason()] = true
	}
	if !reasons[AttentionCorruptBinding] ||
		!reasons[AttentionUnknownChildren] {
		t.Fatalf("malformed inventory attention = %+v", snapshot.Attention())
	}
	plan := discardPlanForSnapshot(t, snapshot)
	if plan.ExpectedStatus() != DiscardNeedsAttention || len(plan.Actions()) != 0 {
		t.Fatalf("ambiguous ownership plan = %v %v", plan.ExpectedStatus(), actionKinds(plan.Actions()))
	}
	if !memoryFileExists(malformedShard, malformedName) ||
		!memoryFileExists(orphanStageShard, stageName) ||
		!memoryFileExists(orphanAnchorShard, anchorName) {
		t.Fatal("read-only ambiguity scan mutated retained state")
	}
	if directoryNamesCalls(intentDirectory) == 0 {
		t.Fatal("leased intent was not inspected")
	}
	resumeClosureClose(t, inventory, leased)
}

func TestResumeClosureObservationHandlesPinRacesAndBoundedOverflow(t *testing.T) {
	t.Run("canonical record replacement during read", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x41)
		tree := newResumeClosureTree()
		tree.behavior(fixture.recordShard).openFile = func(
			name string, private, writable bool,
		) (outputcap.File, error) {
			file, err := fixture.recordShard.OpenFile(name, private, writable)
			if err == nil && name == fixture.recordName {
				fixture.recordShard.mu.Lock()
				fixture.recordShard.files[name] = &memoryFileData{bytes: []byte("replacement")}
				fixture.recordShard.mu.Unlock()
			}
			return file, err
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)

		inventory, leased, snapshot := resumeClosureObserveWithConfig(t, config)
		if len(snapshot.Checkpoints()) != 0 || len(snapshot.Attention()) == 0 ||
			snapshot.Attention()[0].Reason() != AttentionReplacement {
			t.Fatalf("record read race snapshot = %+v", snapshot)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("owned artifact replacement during open", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x42)
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).openFile = func(
			name string, private, writable bool,
		) (outputcap.File, error) {
			file, err := fixture.stageShard.OpenFile(name, private, writable)
			if err == nil && name == fixture.stageName {
				fixture.stageShard.mu.Lock()
				fixture.stageShard.files[name] = &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
				fixture.stageShard.mu.Unlock()
			}
			return file, err
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)

		inventory, leased, snapshot := resumeClosureObserveWithConfig(t, config)
		if len(snapshot.Checkpoints()) != 1 || len(snapshot.Attention()) == 0 ||
			snapshot.Checkpoints()[0].StageEvidence() != EvidenceReplaced {
			t.Fatalf("artifact open race snapshot = %+v", snapshot)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("record close error remains state io", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x43)
		closeErr := errors.New("record handle close failed")
		tree := newResumeClosureTree()
		tree.behavior(fixture.recordShard).openFile = func(
			name string, private, writable bool,
		) (outputcap.File, error) {
			file, err := fixture.recordShard.OpenFile(name, private, writable)
			if err != nil {
				return file, err
			}
			return &resumeClosureFile{File: file, closeErr: closeErr}, nil
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		leased, err := inventory.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leased.Observe(context.Background()); !errors.Is(err, closeErr) {
			t.Fatalf("record close projection = %v", err)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("missing owned directory", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x44)
		fixture.intentDirectory.mu.Lock()
		delete(fixture.intentDirectory.dirs, checkpointstore.StagesDirectory)
		fixture.intentDirectory.mu.Unlock()

		inventory, leased, snapshot := observeResumeFixture(t, fixture)
		if len(snapshot.Checkpoints()) != 0 || len(snapshot.Attention()) == 0 ||
			snapshot.Attention()[0].Reason() != AttentionMissingOwnership {
			t.Fatalf("missing layout snapshot = %+v", snapshot)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("shard directory bound", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x45)
		_, _, records, _, _ := resumeClosureIntentLayout(t, fixture)
		tree := newResumeClosureTree()
		tree.behavior(records).names = func(int) ([]string, error) {
			return make([]string, checkpointstore.ShardLimit), nil
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)

		inventory, leased, snapshot := resumeClosureObserveWithConfig(t, config)
		if len(snapshot.Attention()) == 0 ||
			snapshot.Attention()[0].Reason() != AttentionUnknownChildren {
			t.Fatalf("shard overflow snapshot = %+v", snapshot)
		}
		resumeClosureClose(t, inventory, leased)
	})

	t.Run("scan budgets fail closed without large fixtures", func(t *testing.T) {
		canonical := resumeScanBudget{canonical: checkpointmodel.MaxCheckpointRecordsPerIntent}
		if err := canonical.observe(true); !errors.Is(err, errResumeUnknownChildren) {
			t.Fatalf("canonical budget error = %v", err)
		}
		auxiliary := resumeScanBudget{auxiliary: checkpointmodel.MaxCheckpointAuxiliaryEntriesPerIntent}
		if err := auxiliary.observe(false); !errors.Is(err, errResumeUnknownChildren) {
			t.Fatalf("auxiliary budget error = %v", err)
		}
	})
}

func TestResumeClosureAcquireCancellationAndReopenFailuresReleaseTheLease(t *testing.T) {
	t.Run("cancellation after lock acquisition", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x51)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		leases := checkpointRoot.dirsForTest(t, checkpointstore.LeasesDirectory)
		ctx, cancel := context.WithCancel(context.Background())
		tree := newResumeClosureTree()
		tree.behavior(leases).acquireLock = func(
			name string, existingOnly bool,
		) (outputcap.Lock, bool, error) {
			lock, created, err := leases.AcquireLock(name, existingOnly)
			cancel()
			return lock, created, err
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if leased, err := inventory.Acquire(ctx, 0); leased != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled acquire = %T, %v", leased, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}

		cleanInventory, err := mustResumeRepository(t, fixture.config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		leased, err := cleanInventory.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatalf("canceled acquisition retained the lease: %v", err)
		}
		resumeClosureClose(t, cleanInventory, leased)
	})

	t.Run("reopen and release errors are both retained", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x52)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		leases := checkpointRoot.dirsForTest(t, checkpointstore.LeasesDirectory)
		reopenErr := errors.New("namespace root inspection failed")
		releaseErr := errors.New("lease release failed")
		inspections := 0
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).names = func(limit int) ([]string, error) {
			inspections++
			if inspections == 2 {
				return nil, reopenErr
			}
			return checkpointRoot.Names(limit)
		}
		tree.behavior(leases).acquireLock = func(
			name string, existingOnly bool,
		) (outputcap.Lock, bool, error) {
			lock, created, err := leases.AcquireLock(name, existingOnly)
			if err != nil {
				return lock, created, err
			}
			return &resumeClosureLock{Lock: lock, closeErr: releaseErr}, created, nil
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		leased, err := inventory.Acquire(context.Background(), 0)
		if leased != nil || !errors.Is(err, reopenErr) || !errors.Is(err, releaseErr) {
			t.Fatalf("reopen/release aggregation = %T, %v", leased, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestResumeClosureAcquireAndObservePreserveDependencyFailures(t *testing.T) {
	t.Run("canceled after current root is pinned", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x53)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		ctx, cancel := context.WithCancel(context.Background())
		inspections := 0
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).names = func(limit int) ([]string, error) {
			inspections++
			names, err := checkpointRoot.Names(limit)
			if inspections == 2 {
				cancel()
			}
			return names, err
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if leased, err := inventory.Acquire(ctx, 0); leased != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("post-root cancellation = %T, %v", leased, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("listed root lineage comparison", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x54)
		checks := 0
		tree := newResumeClosureTree()
		tree.behavior(fixture.root).entryMatch = func(
			name string, expected outputcap.CurrentEntryReference,
		) (bool, error) {
			checks++
			if checks == 5 {
				return false, errResumeClosureMatch
			}
			return fixture.root.EntryMatches(name, expected)
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if leased, err := inventory.Acquire(context.Background(), 0); leased != nil || !errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("lineage comparison failure = %T, %v", leased, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("selected intent pin comparison", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x55)
		_, intents, _, _, _ := resumeClosureIntentLayout(t, fixture)
		checks := 0
		tree := newResumeClosureTree()
		tree.behavior(intents).entryMatch = func(
			name string, expected outputcap.CurrentEntryReference,
		) (bool, error) {
			checks++
			if checks == 2 {
				return false, errResumeClosureMatch
			}
			return intents.EntryMatches(name, expected)
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if leased, err := inventory.Acquire(context.Background(), 0); leased != nil || !errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("selected pin comparison failure = %T, %v", leased, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("selected intent pinned open", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x56)
		_, intents, _, _, _ := resumeClosureIntentLayout(t, fixture)
		tree := newResumeClosureTree()
		tree.behavior(intents).openPinned = func(
			outputcap.CurrentEntryReference, bool,
		) (outputcap.Directory, error) {
			return nil, errResumeClosureOpen
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		leased, err := inventory.Acquire(context.Background(), 0)
		if err != nil || leased == nil {
			t.Fatalf("selected pinned-open ambiguity = %T, %v", leased, err)
		}
		snapshot, err := leased.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.NamespaceEvidence() != EvidenceAmbiguous {
			t.Fatalf("selected pinned-open evidence = %v", snapshot.NamespaceEvidence())
		}
		if err := errors.Join(leased.Close(), inventory.Close()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("namespace revalidation", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x57)
		tree := newResumeClosureTree()
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		inventory, repository := resumeClosureAcquire(t, config)
		tree.behavior(fixture.root).entryMatch = func(
			string, outputcap.CurrentEntryReference,
		) (bool, error) {
			return false, errResumeClosureMatch
		}
		if _, err := repository.Observe(context.Background()); !errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("namespace revalidation error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("intent layout second inspection", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x58)
		calls := 0
		tree := newResumeClosureTree()
		tree.behavior(fixture.intentDirectory).names = func(limit int) ([]string, error) {
			calls++
			if calls == 2 {
				return nil, errResumeClosureNames
			}
			return fixture.intentDirectory.Names(limit)
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		if err := resumeClosureObservationError(t, config); !errors.Is(err, errResumeClosureNames) {
			t.Fatalf("intent layout error = %v", err)
		}
	})

	for _, target := range []struct {
		name      string
		directory func(resumeAdapterFixture) *memoryDirectory
	}{
		{
			name: "record shards",
			directory: func(fixture resumeAdapterFixture) *memoryDirectory {
				_, _, records, _, _ := resumeClosureIntentLayout(t, fixture)
				return records
			},
		},
		{
			name: "stage shards",
			directory: func(fixture resumeAdapterFixture) *memoryDirectory {
				_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
				return stages
			},
		},
		{
			name: "anchor shards",
			directory: func(fixture resumeAdapterFixture) *memoryDirectory {
				_, _, _, _, anchors := resumeClosureIntentLayout(t, fixture)
				return anchors
			},
		},
	} {
		t.Run(target.name+" enumeration", func(t *testing.T) {
			fixture := newResumeAdapterFixture(t, 0x59)
			tree := newResumeClosureTree()
			tree.behavior(target.directory(fixture)).names =
				func(int) ([]string, error) { return nil, errResumeClosureNames }
			config := fixture.config
			config.Root = tree.wrap(fixture.root)
			if err := resumeClosureObservationError(t, config); !errors.Is(err, errResumeClosureNames) {
				t.Fatalf("%s enumeration error = %v", target.name, err)
			}
		})
	}

	t.Run("artifact size observation", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0x5a)
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).openFile = func(
			name string, private, writable bool,
		) (outputcap.File, error) {
			file, err := fixture.stageShard.OpenFile(name, private, writable)
			if err != nil {
				return file, err
			}
			return &resumeClosureFile{File: file, sizeErr: errResumeClosureSize}, nil
		}
		config := fixture.config
		config.Root = tree.wrap(fixture.root)
		if err := resumeClosureObservationError(t, config); !errors.Is(err, errResumeClosureSize) {
			t.Fatalf("artifact size observation error = %v", err)
		}
	})
}
