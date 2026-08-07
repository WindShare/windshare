package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentAnchorObservationClassifiesFixedNamespaceEvidence(t *testing.T) {
	failure := errors.New("anchor observation failed")
	for _, test := range []struct {
		name    string
		plan    currentRuntimeFaultPlan
		want    resumestate.AnchorObservation
		wantErr bool
	}{
		{name: "missing shard", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyKind: outputcap.EntryAbsent, classifyExact: true}, want: resumestate.AnchorMissing},
		{name: "ordinary shard failure", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyErr: failure}, wantErr: true},
		{name: "unsafe shard failure", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyErr: outputcap.ErrUnsafeNamespace}, want: resumestate.AnchorUnsafe},
		{name: "ordinary entry failure", plan: currentRuntimeFaultPlan{observeAt: 1, observeErr: failure}, wantErr: true},
		{name: "unsafe entry failure", plan: currentRuntimeFaultPlan{observeAt: 1, observeErr: outputcap.ErrUnsafeNamespace}, want: resumestate.AnchorUnsafe},
		{name: "missing entry", plan: currentRuntimeFaultPlan{observeKindAt: 1, observeKind: outputcap.EntryAbsent}, want: resumestate.AnchorMissing},
		{name: "non-regular entry", plan: currentRuntimeFaultPlan{observeKindAt: 1, observeKind: outputcap.EntryDirectory}, want: resumestate.AnchorUnsafe},
		{name: "open failure", plan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: failure}, want: resumestate.AnchorUnsafe},
		{name: "size failure", plan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeErr: failure}, want: resumestate.AnchorUnsafe},
		{name: "size mismatch", plan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeAdjust: 1}, want: resumestate.AnchorUnsafe},
		{name: "verified", want: resumestate.AnchorVerified},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := currentFaultReadyTransaction(t)
			original := session.inner.anchorsDir
			plan := test.plan
			session.inner.anchorsDir = currentWrapFaultDirectory(original, &plan)
			defer func() { session.inner.anchorsDir = original }()

			file, directory, got, err := session.inner.observeAnchor(transaction.resumable.BoundState().State())
			if closeErr := errors.Join(closeOutputFile(file), closeOutputDirectory(directory)); closeErr != nil &&
				!errors.Is(closeErr, failure) {
				t.Fatalf("close observation: %v", closeErr)
			}
			if got != test.want || (err != nil) != test.wantErr {
				t.Fatalf("anchor observation = (%d, %v), want (%d, error:%t)", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestCurrentStageObservationClassifiesFixedNamespaceEvidence(t *testing.T) {
	failure := errors.New("stage observation failed")
	for _, test := range []struct {
		name        string
		plan        currentRuntimeFaultPlan
		anchorState resumestate.AnchorObservation
		want        resumestate.EntryObservation
		wantErr     bool
	}{
		{name: "missing shard", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyKind: outputcap.EntryAbsent, classifyExact: true}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryMissing},
		{name: "ordinary shard failure", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyErr: failure}, anchorState: resumestate.AnchorVerified, wantErr: true},
		{name: "unsafe shard failure", plan: currentRuntimeFaultPlan{classifyAt: 1, classifyErr: outputcap.ErrUnsafeNamespace}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "ordinary entry failure", plan: currentRuntimeFaultPlan{observeAt: 1, observeErr: failure}, anchorState: resumestate.AnchorVerified, wantErr: true},
		{name: "unsafe entry failure", plan: currentRuntimeFaultPlan{observeAt: 1, observeErr: outputcap.ErrUnsafeNamespace}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "missing entry", plan: currentRuntimeFaultPlan{observeKindAt: 1, observeKind: outputcap.EntryAbsent}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryMissing},
		{name: "different entry", plan: currentRuntimeFaultPlan{observeKindAt: 1, observeKind: outputcap.EntryDirectory}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryDifferentFromAnchor},
		{name: "unresolved entry", plan: currentRuntimeFaultPlan{observeKindAt: 1, observeKind: outputcap.EntryDirectory}, anchorState: resumestate.AnchorMissing, want: resumestate.EntryPresentUnresolved},
		{name: "open failure", plan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: failure}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "unresolved regular file", anchorState: resumestate.AnchorMissing, want: resumestate.EntryPresentUnresolved},
		{name: "identity failure", plan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameErr: failure}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryUnsafe},
		{name: "different file", plan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameFalse: true}, anchorState: resumestate.AnchorVerified, want: resumestate.EntryDifferentFromAnchor},
		{name: "matching file", anchorState: resumestate.AnchorVerified, want: resumestate.EntrySameAsAnchor},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := currentFaultReadyTransaction(t)
			original := session.inner.stagesDir
			plan := test.plan
			session.inner.stagesDir = currentWrapFaultDirectory(original, &plan)
			defer func() { session.inner.stagesDir = original }()

			file, directory, got, err := session.inner.observeStage(
				transaction.resumable.BoundState().State(), transaction.anchor.file, test.anchorState,
			)
			if closeErr := errors.Join(closeOutputFile(file), closeOutputDirectory(directory)); closeErr != nil &&
				!errors.Is(closeErr, failure) {
				t.Fatalf("close observation: %v", closeErr)
			}
			if got != test.want || (err != nil) != test.wantErr {
				t.Fatalf("stage observation = (%d, %v), want (%d, error:%t)", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestCurrentPublicationWitnessRejectsEveryBrokenProof(t *testing.T) {
	failure := errors.New("publication witness failed")
	for _, test := range []struct {
		name       string
		nilStage   bool
		nilAnchor  bool
		stagePlan  currentRuntimeFaultPlan
		anchorPlan currentRuntimeFaultPlan
		expected   bool
		wantErr    bool
		wantUnsafe bool
	}{
		{name: "missing stage directory", nilStage: true, wantErr: true, wantUnsafe: true},
		{name: "missing anchor directory", nilAnchor: true, wantErr: true, wantUnsafe: true},
		{name: "ordinary stage open failure", stagePlan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: failure}, wantErr: true},
		{name: "missing stage", stagePlan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: fs.ErrNotExist}, wantErr: true, wantUnsafe: true},
		{name: "unsafe stage", stagePlan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: outputcap.ErrUnsafeNamespace}, wantErr: true, wantUnsafe: true},
		{name: "ordinary anchor open failure", anchorPlan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: failure}, wantErr: true},
		{name: "missing anchor", anchorPlan: currentRuntimeFaultPlan{openFileAt: 1, openFileErr: fs.ErrNotExist}, wantErr: true, wantUnsafe: true},
		{name: "stage size failure", stagePlan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeErr: failure}, wantErr: true},
		{name: "unsafe anchor size", anchorPlan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeErr: outputcap.ErrUnsafeNamespace}, wantErr: true, wantUnsafe: true},
		{name: "stage size mismatch", stagePlan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeAdjust: 1}, wantErr: true, wantUnsafe: true},
		{name: "anchor size mismatch", anchorPlan: currentRuntimeFaultPlan{fileSizeAt: 1, fileSizeAdjust: 1}, wantErr: true, wantUnsafe: true},
		{name: "stage-anchor identity failure", stagePlan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameErr: failure}, wantErr: true},
		{name: "stage-anchor unsafe identity", stagePlan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameErr: outputcap.ErrUnsafeNamespace}, wantErr: true, wantUnsafe: true},
		{name: "stage-anchor mismatch", stagePlan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameFalse: true}, wantErr: true, wantUnsafe: true},
		{name: "retained stage mismatch", stagePlan: currentRuntimeFaultPlan{fileSameAt: 2, fileSameFalse: true}, expected: true, wantErr: true, wantUnsafe: true},
		{name: "retained anchor mismatch", anchorPlan: currentRuntimeFaultPlan{fileSameAt: 1, fileSameFalse: true}, expected: true, wantErr: true, wantUnsafe: true},
		{name: "metadata failure", anchorPlan: currentRuntimeFaultPlan{fileMetadataAt: 1, fileMetadataErr: failure}, wantErr: true},
		{name: "unsafe metadata failure", anchorPlan: currentRuntimeFaultPlan{fileMetadataAt: 1, fileMetadataErr: outputcap.ErrUnsafeNamespace}, wantErr: true, wantUnsafe: true},
		{name: "metadata mismatch", anchorPlan: currentRuntimeFaultPlan{fileMetadataAt: 1, fileMetadataFalse: true}, wantErr: true, wantUnsafe: true},
		{name: "verified witness", expected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, transaction := currentFaultReadyTransaction(t)
			stageDir := currentWrapFaultDirectory(transaction.stageDir, &test.stagePlan)
			anchorDir := currentWrapFaultDirectory(transaction.anchorDir, &test.anchorPlan)
			if test.nilStage {
				stageDir = nil
			}
			if test.nilAnchor {
				anchorDir = nil
			}
			expected := anchorWitness{}
			if test.expected {
				expected = transaction.anchor
			}
			witness, operationErr, cleanupErr := openPublicationWitnessInDirectoriesResult(
				transaction.resumable.BoundState().State(), stageDir, anchorDir, expected,
			)
			if witness != nil {
				cleanupErr = errors.Join(cleanupErr, witness.Close())
			}
			if (operationErr != nil) != test.wantErr {
				t.Fatalf("operation error = %v, want error:%t", operationErr, test.wantErr)
			}
			if test.wantUnsafe && !errors.Is(operationErr, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("operation error = %v, want unsafe namespace", operationErr)
			}
			if cleanupErr != nil && !errors.Is(cleanupErr, failure) &&
				!errors.Is(cleanupErr, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("cleanup error = %v", cleanupErr)
			}
		})
	}
}

func TestCurrentQuarantineInstallationPersistsBeforeReportingCleanup(t *testing.T) {
	cleanupFailure := errors.New("publication witness cleanup failed")
	for _, test := range []struct {
		name        string
		cleanupErr  error
		wantSettled bool
	}{
		{name: "clean witness", wantSettled: true},
		{name: "cleanup failure", cleanupErr: cleanupFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, transaction := currentFaultReadyTransaction(t)
			t.Cleanup(func() {
				if transaction.lifecycle != FileTransactionClosed {
					currentAbandonTransactionAtCrashCut(t, transaction)
				}
			})
			transaction.mu.Lock()
			settlement, settled, err := transaction.installWitnessQuarantineWithCleanupLocked(
				resumestate.QuarantineStageUnsafe,
				"close publication witness",
				test.cleanupErr,
			)
			transaction.mu.Unlock()
			if settled != test.wantSettled || (err != nil) != (test.cleanupErr != nil) {
				t.Fatalf("quarantine result = settled:%t error:%v", settled, err)
			}
			checkpoint := session.inner.incrementalCheckpointByPath["fault.bin"]
			if checkpoint.CommitState() != resumestate.FileCheckpointCommitQuarantined ||
				checkpoint.QuarantineReason() != resumestate.QuarantineStageUnsafe {
				t.Fatalf("quarantine was not durable before return: %+v", checkpoint)
			}
			if test.cleanupErr == nil && settlement.Kind() != transfer.FileQuarantined {
				t.Fatalf("quarantine settlement = %d", settlement.Kind())
			}
		})
	}

	broken := &FileTransaction{session: &Session{}}
	if _, settled, err := broken.installWitnessQuarantineWithCleanupLocked(
		resumestate.QuarantineStageUnsafe, "close broken witness", cleanupFailure,
	); settled || err == nil || !errors.Is(err, cleanupFailure) {
		t.Fatalf("failed quarantine installation = settled:%t error:%v", settled, err)
	}
}

func TestCurrentRetirementQuarantineReturnsDurableTerminalIdentity(t *testing.T) {
	session, transaction := currentFaultReadyTransaction(t)
	t.Cleanup(func() {
		if transaction.lifecycle != FileTransactionClosed {
			currentAbandonTransactionAtCrashCut(t, transaction)
		}
	})
	bound := transaction.resumable.BoundState()
	decision := currentRecoveryDecision(t, bound, resumestate.FileObservation{
		Anchor: resumestate.AnchorMissing,
		Stage:  resumestate.EntryMissing,
		Final:  resumestate.EntryMissing,
	})
	if decision.Action() != resumestate.RecoveryInstallQuarantine {
		t.Fatalf("missing retirement evidence decision = %d", decision.Action())
	}
	step, err := session.inner.installRetirementQuarantine(
		bound, transaction.binding, decision, nil,
	)
	if err != nil || !step.complete || !step.quarantined || step.settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("retirement quarantine = (%+v, %v)", step, err)
	}
	checkpoint := session.inner.incrementalCheckpointByPath["fault.bin"]
	if checkpoint.CommitState() != resumestate.FileCheckpointCommitQuarantined {
		t.Fatalf("retirement quarantine checkpoint = %+v", checkpoint)
	}
}

func TestCheckpointStoreFailureRetainsPendingRangesUntilV1Commit(t *testing.T) {
	session, transaction := currentFaultReadyTransaction(t)
	if err := transaction.WriteRange(context.Background(), 0, []byte{0x7a}); err != nil {
		t.Fatal(err)
	}

	failure := errors.New("checkpoint replacement failed")
	plan := currentRuntimeFaultPlan{replaceFileAt: 1, replaceFileErr: failure}
	original := session.inner.checkpointsDir
	session.inner.checkpointsDir = currentWrapFaultDirectory(original, &plan)
	defer func() { session.inner.checkpointsDir = original }()

	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("first checkpoint error = %v, want injected store failure", err)
	}
	transaction.mu.Lock()
	record := transaction.resumable.BoundState().State()
	pending := transaction.pending.Ranges()
	transaction.mu.Unlock()
	if record.CheckpointGeneration() != 0 || len(record.DurableRanges().Ranges()) != 0 {
		t.Fatalf("volatile checkpoint advanced after store failure: generation=%d ranges=%v",
			record.CheckpointGeneration(), record.DurableRanges().Ranges())
	}
	if len(pending) != 1 || pending[0].Offset != 0 || pending[0].End != 1 {
		t.Fatalf("pending ranges after store failure = %v, want [0,1)", pending)
	}

	plan.replaceFileAt = plan.replaceFileN + 1
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("retry checkpoint error = %v, want repeated store failure", err)
	}
	plan.replaceFileAt = 0
	durable, err := transaction.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	durableRanges := durable.Ranges().Ranges()
	if durable.CheckpointGeneration() != 1 || len(durableRanges) != 1 ||
		durableRanges[0].Offset != 0 || durableRanges[0].End != 1 {
		t.Fatalf("durable checkpoint after successful retry = %+v", durable)
	}
}

func TestCurrentSmallRuntimeBoundariesRemainTotal(t *testing.T) {
	called := 0
	FilesystemOutputTraceFunc(nil).TraceFilesystemOutput(FilesystemOutputTrace{})
	FilesystemOutputTraceFunc(func(FilesystemOutputTrace) { called++ }).TraceFilesystemOutput(FilesystemOutputTrace{})
	if called != 1 {
		t.Fatalf("trace callback count = %d", called)
	}
	(&Authority{}).trace(FilesystemOutputTrace{})
	(*Authority)(nil).trace(FilesystemOutputTrace{})

	session := &Session{
		beginning: make(map[resumestate.LocatorDigest]struct{}),
		active:    make(map[resumestate.LocatorDigest]*FileTransaction),
	}
	session.teardownPoisoned()
	if !session.closed {
		t.Fatal("poison teardown left the owner open")
	}

	rootSpec := newRuntimeTestRootSpec(t)
	platform, err := openOutputRuntimeTestPlatform(rootSpec.path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = platform.Close() }()
	if err := preflightExistingOutputComponent(platform.Root(), "safe-name"); err != nil {
		t.Fatal(err)
	}
}

func currentFaultReadyTransaction(t *testing.T) (*incrementalOutputSession, *FileTransaction) {
	t.Helper()
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x71, 0x72)
	parent := currentCoverageRoot(t, session, intent, 0x73)
	file := currentCoverageFile(t, session, intent, "fault.bin", parent, 0x74, 0x75, 1)
	transaction := currentCoverageTransaction(t, session, file)
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted) })
	return session, transaction
}
