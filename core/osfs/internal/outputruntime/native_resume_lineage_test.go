package outputruntime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeResumeRestartProjectsRevisionConflictWithoutMovingRanges(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0xc1, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := lease.store.Snapshot()
	if len(records) != 1 || len(records[0].VerifiedRanges()) == 0 {
		t.Fatalf("partial checkpoint records = %+v", records)
	}
	original := records[0]
	conflictingRevision := incrementalTestIdentity16[content.FileRevision](0xf1)
	conflict, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  original.OperationID(),
		ReceiveIntentDigest:          original.ReceiveIntentDigest(),
		MaterializationBindingDigest: original.MaterializationBindingDigest(),
		FileID:                       original.FileID(),
		FileRevision:                 conflictingRevision,
		CanonicalPath:                original.CanonicalPath(),
		ExactSize:                    original.ExactSize(),
		MaterializerKind:             original.MaterializerKind(),
		AuthorityRef:                 original.AuthorityRef().Bytes(),
		OwnedObjectID:                original.OwnedObjectID().Bytes(),
		StateGeneration:              1,
		CheckpointGeneration:         0,
		Phase:                        checkpointmodel.PhaseActive,
		CommitState:                  checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.repository.Create(conflict); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	reacquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	restarted := reacquired.(*NativeResumeLease)
	defer restarted.Close()
	snapshot, err := restarted.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot.Items()
	if len(items) != 1 || items[0].State() != resumeauthority.ItemBlocked ||
		items[0].BlockReason() != resumeauthority.ItemBlockRevisionConflict {
		t.Fatalf("restart items = %+v", items)
	}
	summary, err := resumeauthority.ReduceRecovery(snapshot)
	if err != nil || summary.State() != resumeauthority.OperationIncomplete ||
		summary.State() == resumeauthority.OperationNeedsAttention {
		t.Fatalf("restart summary = (%s, %v)", summary.State(), err)
	}

	slots, attention := restarted.store.LineageSnapshot()
	if len(attention) != 0 || len(slots) != 1 ||
		slots[0].Decision() != checkpointmodel.CheckpointLineageDecisionRevisionConflict {
		t.Fatalf("restart lineage = (slots %+v, attention %+v)", slots, attention)
	}
	if _, selected := slots[0].Record(); selected {
		t.Fatal("revision conflict granted selected range authority")
	}
	physical := slots[0].PhysicalRecords()
	if len(physical) != 2 {
		t.Fatalf("physical conflict evidence = %d records", len(physical))
	}
	foundOriginal := false
	for _, record := range physical {
		if record.RecordID() == original.RecordID() {
			foundOriginal = slices.Equal(record.VerifiedRanges(), original.VerifiedRanges())
		}
	}
	if !foundOriginal {
		t.Fatal("restart moved or discarded the original verified ranges")
	}
	if summary.Items()[0].BlockReason() != resumeauthority.ItemBlockRevisionConflict ||
		summary.Items()[0].State() != resumeauthority.ItemBlocked {
		t.Fatal("revision conflict did not remain an item-local settlement")
	}
}

func TestNativeResumeRepositoryAttentionClosesOperationWithoutInventingAnItem(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x4d, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	retainOpaqueCheckpointShard(t, root, fixture.intent.OperationID())

	var events []FilesystemOutputTrace
	repository, err := NewNativeResumeRepository(
		root,
		openOutputRuntimeTestPlatform,
		FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
			if event.Operation == TraceCheckpointReconciled {
				events = append(events, event)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record := snapshot.Header().Record()
	items := snapshot.Items()
	if record.Lifecycle() != checkpointmodel.OrdinaryOperationNeedsAttention ||
		record.ClosedReason() != checkpointmodel.OrdinaryReasonOperationOwnershipUnknown ||
		len(items) != 1 || items[0].CanonicalPath() == "" ||
		items[0].State() != resumeauthority.ItemResumable {
		t.Fatalf("repository attention snapshot = lifecycle %s reason %s items %+v",
			record.Lifecycle(), record.ClosedReason(), items)
	}
	summary, err := resumeauthority.ReduceRecovery(snapshot)
	if err != nil || summary.State() != resumeauthority.OperationNeedsAttention ||
		summary.NeedsAttentionReason() != checkpointmodel.OrdinaryReasonOperationOwnershipUnknown {
		t.Fatalf("repository attention summary = state %s reason %s err %v",
			summary.State(), summary.NeedsAttentionReason(), err)
	}
	if len(events) != 1 || !events[0].Failed ||
		events[0].RuntimeComponent != FilesystemOutputRuntimeCheckpoint ||
		events[0].RuntimeOperation != FilesystemOutputRuntimeReconcileCheckpoints ||
		events[0].RuntimeDecision != FilesystemOutputRuntimeNeedsAttention ||
		events[0].ReceiveOperationID != fixture.intent.OperationID() ||
		events[0].CheckpointRecordCount != 1 ||
		events[0].CheckpointDecision != 0 || events[0].FailureStage != 0 ||
		events[0].ReconciliationStep != 0 || events[0].NativeErrorClass != 0 ||
		events[0].FaultDomain != 0 || events[0].NormalizedFaultScope != 0 ||
		events[0].NormalizedFaultCode != 0 {
		t.Fatalf("closed repository attention trace = %+v", events)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeReopenRejectsRepositoryAttentionBeforeOutputSession(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x4e, 4)
	pauseOrdinaryResumeFixture(t, fixture)
	retainOpaqueCheckpointShard(t, root, fixture.intent.OperationID())

	var events []FilesystemOutputTrace
	authority := newNativeReservationTestAuthority(t, root)
	authority.tracer = FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		if event.Operation == TraceCheckpointReconciled {
			events = append(events, event)
		}
	})
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), fixture.intent.SelectionSpec())
	if err != nil || lookup.Kind() != ActiveLookupReopened {
		t.Fatalf("attention lookup = kind %d err %v", lookup.Kind(), err)
	}
	if _, err := authority.OpenOperation(context.Background(), lookup.Operation()); err == nil {
		t.Fatal("repository attention admitted a mutable output session")
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].Failed ||
		events[0].RuntimeDecision != FilesystemOutputRuntimeNeedsAttention ||
		events[0].SessionID.IsZero() {
		t.Fatalf("reopen repository attention trace = %+v", events)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := resumeauthority.New(repository)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resume.List(context.Background())
	if err != nil || len(inventory.Summaries()) != 1 {
		t.Fatalf("attention inventory = %+v, %v", inventory.Summaries(), err)
	}
	summary := inventory.Summaries()[0]
	if summary.State() != resumeauthority.OperationNeedsAttention ||
		summary.NeedsAttentionReason() != checkpointmodel.OrdinaryReasonOperationOwnershipUnknown {
		t.Fatalf("attention inventory summary = state %s reason %s",
			summary.State(), summary.NeedsAttentionReason())
	}
}

func TestOrdinaryResumeItemReducesEveryDurableFilePhase(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x78, 4)
	if err := fixture.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := repository.Acquire(context.Background(), fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	lease := acquired.(*NativeResumeLease)
	defer lease.Close()
	if _, err := lease.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := lease.store.Snapshot()
	if len(records) != 1 {
		t.Fatalf("resume records = %d", len(records))
	}
	base := records[0]
	missingObjectBytes := base.OwnedObjectID().Bytes()
	missingObjectBytes[0] ^= 0xff
	missingObject, err := checkpointmodel.ObjectIDFromBytes(missingObjectBytes)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		phase      checkpointmodel.Phase
		commit     checkpointmodel.CommitState
		ranges     []checkpointmodel.Range
		object     checkpointmodel.ObjectID
		generation uint64
		quarantine checkpointmodel.QuarantineReason
		origin     checkpointmodel.QuarantineOrigin
		retirement checkpointmodel.RetirementReason
		want       resumeauthority.ItemState
		block      resumeauthority.ItemBlockReason
	}{
		{
			name: "publishing with durable object", phase: checkpointmodel.PhasePublishing,
			commit: checkpointmodel.CommitVerified, ranges: base.VerifiedRanges(),
			want: resumeauthority.ItemResumable,
		},
		{
			name: "published without exact final", phase: checkpointmodel.PhasePublished,
			commit: checkpointmodel.CommitPublished, ranges: base.VerifiedRanges(),
			want: resumeauthority.ItemBlocked, block: resumeauthority.ItemBlockPublicationUnknown,
		},
		{
			name: "retired isolated failure", phase: checkpointmodel.PhaseRetired,
			commit: checkpointmodel.CommitVerified, ranges: base.VerifiedRanges(),
			retirement: checkpointmodel.RetirementIsolatedFailure,
			want:       resumeauthority.ItemFailed,
		},
		{
			name: "quarantined checkpoint", phase: checkpointmodel.PhaseQuarantined,
			commit: checkpointmodel.CommitQuarantined, ranges: base.VerifiedRanges(),
			quarantine: checkpointmodel.QuarantineStageMismatch,
			origin:     checkpointmodel.QuarantineOriginWitnessed,
			want:       resumeauthority.ItemBlocked, block: resumeauthority.ItemBlockCheckpointInvalid,
		},
		{
			name: "fresh active object", phase: checkpointmodel.PhaseActive,
			commit: checkpointmodel.CommitVerified,
			want:   resumeauthority.ItemIncomplete,
		},
		{
			name:  "committed checkpoint with missing owned object",
			phase: checkpointmodel.PhaseActive, commit: checkpointmodel.CommitVerified,
			object: missingObject,
			want:   resumeauthority.ItemBlocked, block: resumeauthority.ItemBlockOwnedObjectUnknown,
		},
		{
			name:  "initial candidate before object creation",
			phase: checkpointmodel.PhaseActive, commit: checkpointmodel.CommitCandidate,
			object: missingObject, generation: 1,
			want: resumeauthority.ItemIncomplete,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := test.object
			if object.IsZero() {
				object = base.OwnedObjectID()
			}
			generation := test.generation
			if generation == 0 {
				generation = base.StateGeneration() + uint64(index) + 1
			}
			checkpointGeneration := base.CheckpointGeneration()
			if test.commit == checkpointmodel.CommitCandidate {
				checkpointGeneration = 0
			}
			record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
				OperationID:                  base.OperationID(),
				ReceiveIntentDigest:          base.ReceiveIntentDigest(),
				MaterializationBindingDigest: base.MaterializationBindingDigest(),
				FileID:                       base.FileID(),
				FileRevision:                 base.FileRevision(),
				CanonicalPath:                base.CanonicalPath(),
				ExactSize:                    base.ExactSize(),
				MaterializerKind:             base.MaterializerKind(),
				AuthorityRef:                 base.AuthorityRef().Bytes(),
				OwnedObjectID:                object.Bytes(),
				StateGeneration:              generation,
				CheckpointGeneration:         checkpointGeneration,
				VerifiedRanges:               test.ranges,
				Phase:                        test.phase,
				CommitState:                  test.commit,
				QuarantineReason:             test.quarantine,
				QuarantineOrigin:             test.origin,
				RetirementReason:             test.retirement,
			})
			if err != nil {
				t.Fatal(err)
			}
			item, err := ordinaryResumeRecordItem(
				context.Background(),
				lease.topLevel,
				lease.store,
				record,
			)
			wantBlock := test.block
			if wantBlock == 0 {
				wantBlock = resumeauthority.ItemBlockNone
			}
			if err != nil || item.State() != test.want || item.BlockReason() != wantBlock {
				t.Fatalf("resume item = (%s, %s, %v), want (%s, %s)",
					item.State(), item.BlockReason(), err, test.want, wantBlock)
			}
		})
	}
}

func retainOpaqueCheckpointShard(
	t *testing.T,
	root string,
	operation receivecontract.OperationID,
) {
	t.Helper()
	recordsPath := filepath.Join(
		root, checkpointstore.ControlDirectory,
		checkpointstore.OrdinaryRegistryDirectoryV1, "operations",
		bytesToHex(operation.Bytes()),
		"files", checkpointstore.CheckpointsDirectory, checkpointstore.RecordsDirectory,
	)
	if err := os.Mkdir(filepath.Join(recordsPath, "opaque-private-name"), 0o700); err != nil {
		t.Fatal(err)
	}
}
