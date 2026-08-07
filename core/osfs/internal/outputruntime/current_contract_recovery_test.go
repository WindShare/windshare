package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentInitialCheckpointCandidateRecoversAfterPromotionCrash(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	intent := currentCoverageIntent(t, rootSpec.path, 0x21, 0x22)
	failure := errors.New("simulated initial checkpoint promotion crash")
	plan := currentRuntimeFaultPlan{replaceFileAt: 1, replaceFileErr: failure}
	faultFactory := func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &currentRuntimeFaultPlatform{Platform: platform, plan: &plan}, nil
	}
	authority := newIncrementalTestAuthority(t, rootSpec.path, faultFactory)
	opened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	first := opened.(*incrementalOutputSession)
	parent := currentCoverageRoot(t, first, intent, 0x23)
	file := currentCoverageFile(t, first, intent, "initial-candidate.bin", parent, 0x24, 0x25, 1)
	if _, err := first.BeginFile(context.Background(), file); !errors.Is(err, failure) {
		t.Fatalf("BeginFile promotion cut error = %v, want injected failure", err)
	}
	checkpoint := firstIncrementalCheckpoint(first.inner)
	if !initialCheckpointCandidate(checkpoint) {
		t.Fatalf("promotion cut checkpoint = state:%d phase:%d generation:%d ranges:%v",
			checkpoint.CommitState(), checkpoint.Phase(), checkpoint.CheckpointGeneration(), checkpoint.VerifiedRanges())
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopenedAuthority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	reopenedValue, err := reopenedAuthority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenedValue.(*incrementalOutputSession)
	reopenedParent := currentCoverageRoot(t, reopened, intent, 0x26)
	if len(reopened.inner.attention) != 0 {
		t.Fatalf("recovered initial candidate needs attention: %+v", reopened.inner.attention)
	}
	recovered := firstIncrementalCheckpoint(reopened.inner)
	if recovered.CommitState() != resumestate.FileCheckpointCommitVerified ||
		recovered.CheckpointGeneration() != 0 || len(recovered.VerifiedRanges()) != 0 {
		t.Fatalf("recovered initial checkpoint = state:%d generation:%d ranges:%v",
			recovered.CommitState(), recovered.CheckpointGeneration(), recovered.VerifiedRanges())
	}
	reopenedFile := currentCoverageFile(t, reopened, intent, file.Path, reopenedParent, 0x24, 0x25, 1)
	transaction := currentCoverageTransaction(t, reopened, reopenedFile)
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentInitialCheckpointTemporaryCandidateRecoversAfterLinkCrash(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	first, intent := currentCoverageSession(t, rootSpec, 0x31, 0x32)
	parent := currentCoverageRoot(t, first, intent, 0x33)
	file := currentCoverageFile(t, first, intent, "initial-temporary.bin", parent, 0x34, 0x35, 1)
	failure := errors.New("simulated initial checkpoint link crash")
	plan := currentRuntimeFaultPlan{
		linkFileAt: 1, linkFileErr: failure,
		removeFileAt: 1, removeFileErr: failure,
	}
	original := first.inner.checkpointsDir
	first.inner.checkpointsDir = currentWrapFaultDirectory(original, &plan)
	if _, err := first.BeginFile(context.Background(), file); !errors.Is(err, failure) {
		t.Fatalf("BeginFile link cut error = %v, want injected failure", err)
	}
	first.inner.checkpointsDir = original
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopened, reopenedIntent := currentCoverageSession(t, rootSpec, 0x31, 0x32)
	if reopenedIntent.Digest() != intent.Digest() {
		t.Fatal("restart fixture changed transfer intent")
	}
	reopenedParent := currentCoverageRoot(t, reopened, reopenedIntent, 0x36)
	if len(reopened.inner.attention) != 0 {
		t.Fatalf("recovered temporary candidate needs attention: %+v", reopened.inner.attention)
	}
	recovered := firstIncrementalCheckpoint(reopened.inner)
	if recovered.CommitState() != resumestate.FileCheckpointCommitVerified ||
		recovered.CheckpointGeneration() != 0 || len(recovered.VerifiedRanges()) != 0 {
		t.Fatalf("recovered temporary checkpoint = state:%d generation:%d ranges:%v",
			recovered.CommitState(), recovered.CheckpointGeneration(), recovered.VerifiedRanges())
	}
	reopenedFile := currentCoverageFile(t, reopened, reopenedIntent, file.Path, reopenedParent, 0x34, 0x35, 1)
	transaction := currentCoverageTransaction(t, reopened, reopenedFile)
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentCheckpointReplacementCandidateRecoversAfterPromotionCrash(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	first, intent := currentCoverageSession(t, rootSpec, 0x27, 0x28)
	parent := currentCoverageRoot(t, first, intent, 0x29)
	file := currentCoverageFile(t, first, intent, "replacement-candidate.bin", parent, 0x2a, 0x2b, 1)
	transaction := currentCoverageTransaction(t, first, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte{0x5a}); err != nil {
		t.Fatal(err)
	}

	failure := errors.New("simulated checkpoint replacement crash")
	plan := currentRuntimeFaultPlan{
		replaceFileAt: 1, replaceFileErr: failure,
		removeFileAt: 1, removeFileErr: failure,
	}
	original := first.inner.checkpointsDir
	first.inner.checkpointsDir = currentWrapFaultDirectory(original, &plan)
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("checkpoint promotion cut error = %v, want injected failure", err)
	}
	first.inner.checkpointsDir = original
	currentAbandonTransactionAtCrashCut(t, transaction)
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopened, reopenedIntent := currentCoverageSession(t, rootSpec, 0x27, 0x28)
	if reopenedIntent.Digest() != intent.Digest() {
		t.Fatal("restart fixture changed transfer intent")
	}
	reopenedParent := currentCoverageRoot(t, reopened, reopenedIntent, 0x2c)
	if len(reopened.inner.attention) != 0 {
		t.Fatalf("recovered replacement candidate needs attention: %+v", reopened.inner.attention)
	}
	recovered := firstIncrementalCheckpoint(reopened.inner)
	if recovered.CommitState() != resumestate.FileCheckpointCommitVerified ||
		recovered.CheckpointGeneration() != 1 || len(recovered.VerifiedRanges()) != 1 {
		t.Fatalf("recovered replacement checkpoint = state:%d generation:%d ranges:%v",
			recovered.CommitState(), recovered.CheckpointGeneration(), recovered.VerifiedRanges())
	}
	checkpointPath := currentCheckpointPrivatePaths(
		rootSpec.path, intent.Digest(), transaction.resumable.BoundState().State(), recovered,
	)[2]
	entries, err := os.ReadDir(filepath.Dir(checkpointPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if checkpointstore.IsTemporaryName(entry.Name()) {
			t.Fatalf("recovered checkpoint retained deterministic candidate %q", entry.Name())
		}
	}

	reopenedFile := currentCoverageFile(t, reopened, reopenedIntent, file.Path, reopenedParent, 0x2a, 0x2b, 1)
	resumed := currentCoverageTransaction(t, reopened, reopenedFile)
	ranges := resumed.resumable.BoundState().State().DurableRanges().Ranges()
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != 1 {
		t.Fatalf("recovered durable ranges = %v, want [0,1)", ranges)
	}
	if _, err := resumed.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentPublishingCrashCutsRecoverFromFileCheckpoint(t *testing.T) {
	payload := []byte("recoverable-payload")
	for index, test := range []struct {
		name       string
		finalState string
		want       transfer.FileSettlementKind
	}{
		{name: "before-final-link", finalState: "missing", want: transfer.FilePublished},
		{name: "after-final-link", finalState: "matching", want: transfer.FilePublished},
		{name: "foreign-final", finalState: "foreign", want: transfer.FileQuarantined},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootSpec := newRuntimeTestRootSpec(t)
			shareByte := byte(0x31 + index*4)
			rootByte := shareByte + 1
			fileByte := shareByte + 2
			revisionByte := shareByte + 3
			first, intent := currentCoverageSession(t, rootSpec, shareByte, rootByte)
			parent := currentCoverageRoot(t, first, intent, 0x70)
			file := currentCoverageFile(
				t, first, intent, "publishing.bin", parent, fileByte, revisionByte, uint64(len(payload)),
			)
			transaction := currentCoverageTransaction(t, first, file)
			if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
				t.Fatal(err)
			}
			if _, err := transaction.Checkpoint(context.Background()); err != nil {
				t.Fatal(err)
			}

			publishing := currentInstallPublishingCut(t, transaction)
			switch test.finalState {
			case "missing":
			case "matching":
				result, operationErr, cleanupErr := first.inner.linkFinalNoReplaceResult(
					publishing, &publicationWitness{stage: transaction.data, anchor: transaction.anchor},
				)
				if result != resumestate.PublishLinkCreated || operationErr != nil || cleanupErr != nil {
					t.Fatalf("install matching final = (%d, %v, %v)", result, operationErr, cleanupErr)
				}
			case "foreign":
				foreign := append([]byte(nil), payload...)
				foreign[0] ^= 0xff
				if err := os.WriteFile(filepath.Join(rootSpec.path, file.Path), foreign, 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unsupported final state %q", test.finalState)
			}
			currentAbandonTransactionAtCrashCut(t, transaction)
			if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}

			reopened, reopenedIntent := currentCoverageSession(t, rootSpec, shareByte, rootByte)
			if reopenedIntent.Digest() != intent.Digest() {
				t.Fatal("restart fixture changed transfer intent")
			}
			reopenedParent := currentCoverageRoot(t, reopened, reopenedIntent, 0x71)
			reopenedFile := currentCoverageFile(
				t, reopened, reopenedIntent, file.Path, reopenedParent,
				fileByte, revisionByte, uint64(len(payload)),
			)
			start, err := reopened.BeginFile(context.Background(), reopenedFile)
			if err != nil {
				t.Fatal(err)
			}
			settlement, settled := start.ImmediateSettlement()
			if !settled || settlement.Kind() != test.want {
				t.Fatalf("recovered settlement = (%d, %t), want %d", settlement.Kind(), settled, test.want)
			}
			if test.want == transfer.FilePublished {
				actual, err := os.ReadFile(filepath.Join(rootSpec.path, file.Path))
				if err != nil || string(actual) != string(payload) {
					t.Fatalf("recovered publication = %q, %v", actual, err)
				}
			}
			if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCurrentRetirementRecoversEveryOrderedRemovalCut(t *testing.T) {
	for removed := 0; removed <= 3; removed++ {
		t.Run(currentRetirementCutName(removed), func(t *testing.T) {
			rootSpec := newRuntimeTestRootSpec(t)
			shareByte := byte(0x51 + removed*4)
			rootByte := shareByte + 1
			fileByte := shareByte + 2
			revisionByte := shareByte + 3
			first, intent := currentCoverageSession(t, rootSpec, shareByte, rootByte)
			parent := currentCoverageRoot(t, first, intent, 0x72)
			file := currentCoverageFile(t, first, intent, "retiring.bin", parent, fileByte, revisionByte, 1)
			transaction := currentCoverageTransaction(t, first, file)

			retiring, err := resumestate.PrepareCheckpointRuntimeIsolatedRetirement(
				transaction.resumable.BoundState(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.inner.installCheckpointRuntimeState(transaction.resumable.BoundState(), retiring); err != nil {
				t.Fatal(err)
			}
			checkpoint := first.inner.incrementalCheckpointByPath[file.Path]
			if checkpoint.RecordID().IsZero() {
				t.Fatal("retirement cut did not retain FileCheckpointV1 authority")
			}
			privatePaths := currentCheckpointPrivatePaths(rootSpec.path, intent.Digest(), retiring.State(), checkpoint)
			currentAbandonTransactionAtCrashCut(t, transaction)
			if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			for _, path := range privatePaths[:removed] {
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove simulated crash-cut object %q: %v", path, err)
				}
			}

			reopened, reopenedIntent := currentCoverageSession(t, rootSpec, shareByte, rootByte)
			reopenedParent := currentCoverageRoot(t, reopened, reopenedIntent, 0x73)
			reopenedFile := currentCoverageFile(
				t, reopened, reopenedIntent, file.Path, reopenedParent, fileByte, revisionByte, 1,
			)
			start, err := reopened.BeginFile(context.Background(), reopenedFile)
			if err != nil {
				t.Fatal(err)
			}
			if removed < len(privatePaths) {
				settlement, settled := start.ImmediateSettlement()
				if !settled || settlement.Kind() != transfer.FileRetired {
					t.Fatalf("retirement recovery = (%d, %t), want retired", settlement.Kind(), settled)
				}
				start, err = reopened.BeginFile(context.Background(), reopenedFile)
				if err != nil {
					t.Fatal(err)
				}
			}
			value, _, active := start.Transaction()
			if !active {
				t.Fatalf("completed retirement did not release a fresh transaction: %+v", start)
			}
			if _, err := value.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
				t.Fatal(err)
			}
			for _, path := range privatePaths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retired private object %q remains: %v", path, err)
				}
			}
			if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCurrentQuarantineCheckpointRestoresTerminalSemantics(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	first, intent := currentCoverageSession(t, rootSpec, 0x75, 0x76)
	parent := currentCoverageRoot(t, first, intent, 0x77)
	file := currentCoverageFile(t, first, intent, "quarantined.bin", parent, 0x78, 0x79, 1)
	transaction := currentCoverageTransaction(t, first, file)

	quarantined, err := resumestate.PrepareCheckpointRuntimeUnsafeNamespaceQuarantine(
		transaction.resumable.BoundState(), resumestate.QuarantineStageUnsafe,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.inner.installCheckpointRuntimeState(transaction.resumable.BoundState(), quarantined); err != nil {
		t.Fatal(err)
	}
	transaction.resumable, err = resumestate.BindCheckpointRuntimeDescriptor(quarantined, transaction.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := first.inner.incrementalCheckpointByPath[file.Path]
	if checkpoint.CommitState() != resumestate.FileCheckpointCommitQuarantined ||
		checkpoint.QuarantineReason() != resumestate.QuarantineStageUnsafe ||
		checkpoint.PhaseBeforeQuarantine() != resumestate.CheckpointRuntimeWitnessed {
		t.Fatalf("quarantine checkpoint lost terminal semantics: phase=%d reason=%d origin=%d",
			checkpoint.CommitState(), checkpoint.QuarantineReason(), checkpoint.PhaseBeforeQuarantine())
	}
	currentAbandonTransactionAtCrashCut(t, transaction)
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopened, reopenedIntent := currentCoverageSession(t, rootSpec, 0x75, 0x76)
	reopenedParent := currentCoverageRoot(t, reopened, reopenedIntent, 0x7a)
	reopenedFile := currentCoverageFile(t, reopened, reopenedIntent, file.Path, reopenedParent, 0x78, 0x79, 1)
	start, err := reopened.BeginFile(context.Background(), reopenedFile)
	if err != nil {
		t.Fatal(err)
	}
	settlement, settled := start.ImmediateSettlement()
	if !settled || settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("restored quarantine settlement = (%d, %t)", settlement.Kind(), settled)
	}
	if _, reason, ok := settlement.Quarantine(); !ok || reason != transfer.QuarantineOwnershipMismatch {
		t.Fatalf("restored quarantine reason = (%d, %t)", reason, ok)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func currentInstallPublishingCut(
	t *testing.T,
	transaction *FileTransaction,
) resumestate.BoundCheckpointRuntimeState {
	t.Helper()
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.data.SetModifiedTime(transaction.descriptor.ModifiedTime()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.data.Sync(); err != nil {
		t.Fatal(err)
	}
	publishing, err := resumestate.PrepareCheckpointRuntimePublication(transaction.resumable)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.session.installCheckpointRuntimeState(transaction.resumable.BoundState(), publishing); err != nil {
		t.Fatal(err)
	}
	transaction.resumable, err = resumestate.BindCheckpointRuntimeDescriptor(publishing, transaction.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return publishing
}

func currentAbandonTransactionAtCrashCut(t *testing.T, transaction *FileTransaction) {
	t.Helper()
	record := transaction.resumable.BoundState().State()
	transaction.mu.Lock()
	transaction.lifecycle = FileTransactionClosed
	closeErr := transaction.closeHandlesLocked()
	transaction.mu.Unlock()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	transaction.session.finishFile(record.LocatorDigest(), transaction)
}

func currentCheckpointPrivatePaths(
	root string,
	intent transfer.TransferIntentDigest,
	record resumestate.CheckpointRuntimeState,
	checkpoint resumestate.FileCheckpointV1,
) []string {
	intentRoot := filepath.Join(
		root,
		resumestate.ControlDirectoryName,
		resumestate.CheckpointsDirectoryName,
		checkpointstore.IntentsDirectory,
		resumestate.IntentNamespaceName(intent),
	)
	stage := resumestate.StageName(record.OutputObject())
	anchor := resumestate.AnchorName(record.OutputObject())
	checkpointName := resumestate.FileCheckpointName(checkpoint.RecordID())
	return []string{
		filepath.Join(intentRoot, resumestate.StagesDirectoryName, stage.Shard(), stage.Name()),
		filepath.Join(intentRoot, resumestate.AnchorsDirectoryName, anchor.Shard(), anchor.Name()),
		filepath.Join(intentRoot, checkpointstore.RecordsDirectory, checkpointName.Shard(), checkpointName.Name()),
	}
}

func currentRetirementCutName(removed int) string {
	return []string{
		"before-stage-removal",
		"after-stage-removal",
		"after-anchor-removal",
		"after-checkpoint-removal",
	}[removed]
}
