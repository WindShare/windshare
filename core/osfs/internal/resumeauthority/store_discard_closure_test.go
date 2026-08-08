package resumeauthority

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestResumeClosureDiscardReconcilesExactRemovalCrashCuts(t *testing.T) {
	t.Run("artifact removed before failure report", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x61)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		base := checkpoint.stage.entry.shard.directory
		removeErr := errors.New("artifact removal result was lost")
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).removeEntry = func(
			name string, expected outputcap.CurrentEntryReference,
		) error {
			if err := base.RemoveEntry(name, expected); err != nil {
				return err
			}
			return removeErr
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)

		result, err := repository.Apply(context.Background(), actions[0])
		if err != nil || result.Status() != ApplyCompleted ||
			memoryFileExists(fixture.stageShard, fixture.stageName) {
			t.Fatalf("removed artifact reconciliation = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact failure while exact", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x62)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		removeErr := errors.New("artifact remove failed")
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).removeEntry = func(
			string, outputcap.CurrentEntryReference,
		) error {
			return removeErr
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)

		if _, err := repository.Apply(context.Background(), actions[0]); !errors.Is(err, removeErr) {
			t.Fatalf("exact artifact removal error = %v", err)
		}
		if !memoryFileExists(fixture.stageShard, fixture.stageName) {
			t.Fatal("failed exact artifact removal mutated state")
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact replaced while removal outcome is unknown", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x63)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		removeErr := errors.New("artifact remove raced")
		replacement := &memoryFileData{bytes: make([]byte, fixture.record.ExactSize())}
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).removeEntry = func(
			name string, _ outputcap.CurrentEntryReference,
		) error {
			fixture.stageShard.mu.Lock()
			fixture.stageShard.files[name] = replacement
			fixture.stageShard.mu.Unlock()
			return removeErr
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)

		result, err := repository.Apply(context.Background(), actions[0])
		if err != nil || result.Status() != ApplyNeedsAttention ||
			result.Attention()[0].Reason() != AttentionReplacement {
			t.Fatalf("raced artifact removal = %v, %v", result.Status(), err)
		}
		fixture.stageShard.mu.Lock()
		retained := fixture.stageShard.files[fixture.stageName] == replacement
		fixture.stageShard.mu.Unlock()
		if !retained {
			t.Fatal("replacement artifact was removed")
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact size changes after observation", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x64)
		fixture.stageShard.mu.Lock()
		data := fixture.stageShard.files[fixture.stageName]
		fixture.stageShard.mu.Unlock()
		data.mu.Lock()
		data.bytes = data.bytes[:len(data.bytes)-1]
		data.mu.Unlock()

		result, err := repository.Apply(context.Background(), actions[0])
		if err != nil || result.Status() != ApplyNeedsAttention ||
			result.Attention()[0].Reason() != AttentionReplacement {
			t.Fatalf("artifact size drift = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("record removed before failure report", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x65)
		applyActions(t, repository, actions[:4])
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		base := checkpoint.entry.shard.directory
		removeErr := errors.New("record removal result was lost")
		tree := newResumeClosureTree()
		tree.behavior(fixture.recordShard).removeEntry = func(
			name string, expected outputcap.CurrentEntryReference,
		) error {
			if err := base.RemoveEntry(name, expected); err != nil {
				return err
			}
			return removeErr
		}
		checkpoint.entry.shard.directory = tree.wrap(fixture.recordShard)

		result, err := repository.Apply(context.Background(), actions[4])
		if err != nil || result.Status() != ApplyCompleted ||
			memoryFileExists(fixture.recordShard, fixture.recordName) {
			t.Fatalf("removed record reconciliation = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("record failure while exact", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x66)
		applyActions(t, repository, actions[:4])
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		removeErr := errors.New("record remove failed")
		tree := newResumeClosureTree()
		tree.behavior(fixture.recordShard).removeEntry = func(
			string, outputcap.CurrentEntryReference,
		) error {
			return removeErr
		}
		checkpoint.entry.shard.directory = tree.wrap(fixture.recordShard)

		if _, err := repository.Apply(context.Background(), actions[4]); !errors.Is(err, removeErr) {
			t.Fatalf("exact record removal error = %v", err)
		}
		if !memoryFileExists(fixture.recordShard, fixture.recordName) {
			t.Fatal("failed exact record removal mutated state")
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("sync failure remains a separate durable cut", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x67)
		if _, err := repository.Apply(context.Background(), actions[0]); err != nil {
			t.Fatal(err)
		}
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		syncErr := errors.New("stage shard sync failed")
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).sync = func() error { return syncErr }
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)

		if _, err := repository.Apply(context.Background(), actions[1]); !errors.Is(err, syncErr) {
			t.Fatalf("stage sync error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("record replacement between removal and sync", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x68)
		applyActions(t, repository, actions[:5])
		fixture.recordShard.mu.Lock()
		replacement := &memoryFileData{bytes: []byte("record replacement")}
		fixture.recordShard.files[fixture.recordName] = replacement
		fixture.recordShard.mu.Unlock()

		result, err := repository.Apply(context.Background(), actions[5])
		if err != nil || result.Status() != ApplyNeedsAttention ||
			result.Attention()[0].Reason() != AttentionReplacement {
			t.Fatalf("record sync replacement = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	for _, boundary := range []struct {
		name    string
		replace func(resumeAdapterFixture)
	}{
		{
			name: "owned directory",
			replace: func(fixture resumeAdapterFixture) {
				fixture.intentDirectory.mu.Lock()
				fixture.intentDirectory.dirs[checkpointstore.StagesDirectory] = newMemoryDirectory()
				fixture.intentDirectory.mu.Unlock()
			},
		},
		{
			name: "owned shard",
			replace: func(fixture resumeAdapterFixture) {
				_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
				shardName, _ := recoveryArtifactLocation(t, fixture.record.OwnedOutputObject(), checkpointstore.RecoveryStage)
				stages.mu.Lock()
				stages.dirs[shardName] = newMemoryDirectory()
				stages.mu.Unlock()
			},
		},
	} {
		t.Run("replacement of "+boundary.name, func(t *testing.T) {
			fixture, inventory, repository, actions := resumeClosureObserved(t, 0x69)
			boundary.replace(fixture)
			result, err := repository.Apply(context.Background(), actions[0])
			if err != nil || result.Status() != ApplyNeedsAttention ||
				result.Attention()[0].Reason() != AttentionReplacement {
				t.Fatalf("%s replacement = %v, %v", boundary.name, result.Status(), err)
			}
			assertResumeArtifactsPresent(t, fixture)
			resumeClosureClose(t, inventory, repository)
		})
	}
}

func TestResumeClosureApplyPreservesPostObservationFaults(t *testing.T) {
	t.Run("namespace replacement", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6a)
		fixture.root.mu.Lock()
		fixture.root.dirs[checkpointstore.ControlDirectory] = newMemoryDirectory()
		fixture.root.mu.Unlock()
		result, err := repository.Apply(context.Background(), actions[0])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("namespace replacement = %v, %v", result.Status(), err)
		}
		assertResumeArtifactsPresent(t, fixture)
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("namespace comparison failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6b)
		_, intents, _, _, _ := resumeClosureIntentLayout(t, fixture)
		tree := newResumeClosureTree()
		tree.behavior(intents).entryMatch = func(
			string, outputcap.CurrentEntryReference,
		) (bool, error) {
			return false, errResumeClosureMatch
		}
		repository.root.intents = tree.wrap(intents)
		if _, err := repository.Apply(context.Background(), actions[0]); !errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("namespace comparison error = %v", err)
		}
		assertResumeArtifactsPresent(t, fixture)
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact disappears before open", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6c)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).openFile = func(
			string, bool, bool,
		) (outputcap.File, error) {
			return nil, fs.ErrNotExist
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)
		result, err := repository.Apply(context.Background(), actions[0])
		if err != nil || result.Status() != ApplyNeedsAttention ||
			result.Attention()[0].Reason() != AttentionReplacement {
			t.Fatalf("disappeared artifact = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact open failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6d)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).openFile = func(
			string, bool, bool,
		) (outputcap.File, error) {
			return nil, errResumeClosureOpen
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)
		if _, err := repository.Apply(context.Background(), actions[0]); !errors.Is(err, errResumeClosureOpen) {
			t.Fatalf("artifact open error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact size failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6e)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).openFile = func(
			name string, private bool, writable bool,
		) (outputcap.File, error) {
			file, err := fixture.stageShard.OpenFile(name, private, writable)
			if err != nil {
				return nil, err
			}
			return &resumeClosureFile{File: file, sizeErr: errResumeClosureSize}, nil
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)
		if _, err := repository.Apply(context.Background(), actions[0]); !errors.Is(err, errResumeClosureSize) {
			t.Fatalf("artifact size error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("artifact removal reconciliation failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x6f)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		removeErr := errors.New("resume closure artifact removal failed")
		checks := 0
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).entryMatch = func(
			name string, expected outputcap.CurrentEntryReference,
		) (bool, error) {
			checks++
			if checks == 4 {
				return false, errResumeClosureMatch
			}
			return fixture.stageShard.EntryMatches(name, expected)
		}
		tree.behavior(fixture.stageShard).removeEntry = func(
			string, outputcap.CurrentEntryReference,
		) error {
			return removeErr
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)
		if _, err := repository.Apply(context.Background(), actions[0]); !errors.Is(err, removeErr) ||
			!errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("artifact reconciliation error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("record image changes in place", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x70)
		applyActions(t, repository, actions[:4])
		fixture.recordShard.mu.Lock()
		data := fixture.recordShard.files[fixture.recordName]
		fixture.recordShard.mu.Unlock()
		data.mu.Lock()
		data.bytes[0] ^= 0xff
		data.mu.Unlock()
		result, err := repository.Apply(context.Background(), actions[4])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("record image drift = %v, %v", result.Status(), err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("record reopen failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x74)
		applyActions(t, repository, actions[:4])
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		tree := newResumeClosureTree()
		tree.behavior(fixture.recordShard).openFile = func(
			string, bool, bool,
		) (outputcap.File, error) {
			return nil, errResumeClosureOpen
		}
		checkpoint.entry.shard.directory = tree.wrap(fixture.recordShard)
		if _, err := repository.Apply(context.Background(), actions[4]); !errors.Is(err, errResumeClosureOpen) {
			t.Fatalf("record reopen error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("removed artifact comparison failure", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x75)
		if _, err := repository.Apply(context.Background(), actions[0]); err != nil {
			t.Fatal(err)
		}
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		tree := newResumeClosureTree()
		tree.behavior(fixture.stageShard).entryMatch = func(
			string, outputcap.CurrentEntryReference,
		) (bool, error) {
			return false, errResumeClosureMatch
		}
		checkpoint.stage.entry.shard.directory = tree.wrap(fixture.stageShard)
		if _, err := repository.Apply(context.Background(), actions[1]); !errors.Is(err, errResumeClosureMatch) {
			t.Fatalf("removed artifact comparison error = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("missing internal lineage", func(t *testing.T) {
		repository := &resumeLeasedRepository{}
		if _, err := repository.revalidateEntryLineage(resumeEntryPins{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("missing entry lineage = %v", err)
		}
		if _, err := repository.revalidateShardLineage(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("missing shard lineage = %v", err)
		}
	})
}

func TestResumeClosureApplyRejectsUntrustedStaleAndExhaustedPlans(t *testing.T) {
	t.Run("invalid and exhausted plans", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0x71)
		if _, err := repository.Apply(context.Background(), Action{}); err == nil {
			t.Fatal("invalid action was accepted")
		}
		applyActions(t, repository, actions)
		if _, err := repository.Apply(context.Background(), actions[len(actions)-1]); err == nil {
			t.Fatal("exhausted plan accepted another action")
		}
		assertResumeArtifactsAbsent(t, fixture)
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("snapshot attention blocks a matching action", func(t *testing.T) {
		target := newResumeAdapterFixture(t, 0x72)
		target.intentDirectory.mu.Lock()
		target.intentDirectory.files["future.child"] = &memoryFileData{bytes: []byte("opaque")}
		target.intentDirectory.mu.Unlock()
		targetInventory, targetRepository, targetSnapshot := observeResumeFixture(t, target)
		if targetSnapshot.NamespaceEvidence() == EvidenceExact {
			t.Fatal("target snapshot unexpectedly trusted unknown state")
		}

		_, sourceInventory, sourceRepository, sourceActions := resumeClosureObserved(t, 0x72)
		result, err := targetRepository.Apply(context.Background(), sourceActions[0])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("attention-gated apply = %v, %v", result.Status(), err)
		}
		assertResumeArtifactsPresent(t, target)
		resumeClosureClose(t, targetInventory, targetRepository)
		resumeClosureClose(t, sourceInventory, sourceRepository)
	})

	t.Run("same record id with a different committed image", func(t *testing.T) {
		target, targetInventory, targetRepository, targetActions := resumeClosureObserved(t, 0x73)
		source := newResumeAdapterFixture(t, 0x73)
		advanced, err := checkpointmodel.AdvanceGeneration(
			source.record,
			[]checkpointmodel.Range{{Offset: 0, End: 16}},
			checkpointmodel.PhaseActive,
			checkpointmodel.CommitVerified,
		)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := checkpointmodel.EncodeRecord(advanced)
		if err != nil {
			t.Fatal(err)
		}
		source.recordShard.mu.Lock()
		source.recordShard.files[source.recordName] = &memoryFileData{bytes: encoded}
		source.recordShard.mu.Unlock()
		source.record = advanced
		sourceInventory, sourceRepository, sourceSnapshot := observeResumeFixture(t, source)
		sourceActions := discardPlanForSnapshot(t, sourceSnapshot).Actions()
		if sourceActions[0].RecordID() != targetActions[0].RecordID() {
			t.Fatal("image-mismatch fixture changed immutable record identity")
		}

		if _, err := targetRepository.Apply(context.Background(), sourceActions[0]); err == nil {
			t.Fatal("foreign committed image was accepted for a matching record id")
		}
		assertResumeArtifactsPresent(t, target)
		resumeClosureClose(t, targetInventory, targetRepository)
		resumeClosureClose(t, sourceInventory, sourceRepository)
	})
}

func TestResumeClosureCloseAggregatesPinsBeforeReleasingTheLease(t *testing.T) {
	log := make([]string, 0)
	recordErr := errors.New("record pin close failed")
	artifactErr := errors.New("artifact pin close failed")
	rootErr := errors.New("root close failed")
	leaseErr := errors.New("lease lock close failed")
	recordID := mustRecordID(t, 0x81)
	repository := &resumeLeasedRepository{
		checkpointPins: map[checkpointmodel.RecordID]*resumeCheckpointPins{
			recordID: {entry: resumeEntryPins{pin: &resumeClosureTrackedReference{
				name: "record-pin", log: &log, closeErr: recordErr,
			}}},
		},
		artifactPins: []*resumeArtifactPins{{
			entry: resumeEntryPins{pin: &resumeClosureTrackedReference{
				name: "artifact-pin", log: &log, closeErr: artifactErr,
			}},
		}},
		root: &resumeRootPins{root: &resumeClosureTrackedDirectory{
			name: "root", log: &log, closeErr: rootErr,
		}},
		lease: resumeClosureLease{lock: &resumeClosureTrackedLock{
			name: "lease-lock", log: &log, closeErr: leaseErr,
		}},
	}
	err := repository.Close()
	for _, expected := range []error{recordErr, artifactErr, rootErr, leaseErr} {
		if !errors.Is(err, expected) {
			t.Fatalf("close error %v missing from %v", expected, err)
		}
	}
	if len(log) == 0 || log[len(log)-1] != "lease-lock" {
		t.Fatalf("close order = %v", log)
	}
	closedCount := len(log)
	if repeated := repository.Close(); !errors.Is(repeated, leaseErr) || len(log) != closedCount {
		t.Fatalf("repeated close = %v log=%v", repeated, log)
	}

	inventoryPinErr := errors.New("inventory intent pin close failed")
	inventoryRootErr := errors.New("inventory root close failed")
	inventory := newResumeInventory(checkpointstore.CertifiedConfig{}, &resumeRootPins{
		root: &resumeClosureTrackedDirectory{name: "inventory-root", log: &log, closeErr: inventoryRootErr},
	})
	inventory.items = []resumeInventoryItem{{
		pin: &resumeClosureTrackedReference{name: "inventory-pin", log: &log, closeErr: inventoryPinErr},
	}}
	err = inventory.Close()
	if !errors.Is(err, inventoryPinErr) || !errors.Is(err, inventoryRootErr) {
		t.Fatalf("inventory close aggregation = %v", err)
	}
	inventoryClosedCount := len(log)
	if repeated := inventory.Close(); !errors.Is(repeated, inventoryRootErr) || len(log) != inventoryClosedCount {
		t.Fatalf("repeated inventory close = %v log=%v", repeated, log)
	}
}
