package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestIncrementalBeginFileRequiresAdmittedParentAndUsesLiveCheckpointOverlay(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	share := incrementalTestIdentity16[catalog.ShareInstance](0x61)
	root := incrementalTestIdentity16[catalog.DirectoryID](0x62)
	generation := incrementalTestIdentity16[catalog.DirectoryGeneration](0x63)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		share, root, rules, rootSpec.path, filesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := opened.(*incrementalOutputSession)
	if !ok {
		t.Fatalf("OpenOutput returned %T, want incremental session", opened)
	}
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseShutdown) })

	fileID := incrementalTestIdentity16[catalog.FileID](0x64)
	revision := incrementalTestIdentity16[content.FileRevision](0x65)
	geometry, err := content.NewFileGeometry(4, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, fileID, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := outputTargetForDescriptor(session.SessionID(), descriptor, "live.bin")
	if err != nil {
		t.Fatal(err)
	}
	unadmitted := transfer.OutputFile{Path: "live.bin", ExpectedSize: 4, Descriptor: descriptor, Target: target}
	if _, err := session.BeginFile(context.Background(), unadmitted); !errors.Is(err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("BeginFile before parent admission error = %v", err)
	}

	rootAdmission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: root, Generation: generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := unadmitted
	file.ParentAdmission = rootAdmission
	foreignTarget, err := outputTargetForDescriptor(
		incrementalTestIdentity16[transfer.OutputSessionID](0x66), descriptor, file.Path,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongTarget := file
	wrongTarget.Target = foreignTarget
	if _, err := session.BeginFile(context.Background(), wrongTarget); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("foreign session target error = %v", err)
	}
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("BeginFile after root admission: %v", err)
	}
	transactionValue, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("BeginFile returned no transaction: %+v", start)
	}
	transaction, ok := transactionValue.(*FileTransaction)
	if !ok {
		t.Fatalf("BeginFile transaction type = %T", transactionValue)
	}
	if len(session.inner.incrementalCheckpoints) != 1 {
		t.Fatalf("incremental checkpoint count = %d, want 1", len(session.inner.incrementalCheckpoints))
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatalf("checkpoint dynamic file: %v", err)
	}
	checkpoint := firstIncrementalCheckpoint(session.inner)
	if checkpoint.CommitState() != resumestate.FileCheckpointCommitVerified {
		t.Fatalf("checkpoint commit state = %v, want verified", checkpoint.CommitState())
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: root, Generation: generation,
	}); err == nil {
		t.Fatal("FinalizeDirectory succeeded while dynamic file transaction was active")
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatalf("pause dynamic file: %v", err)
	}
	wrongParent := file
	wrongParent.ParentAdmission = transfer.DirectoryAdmission{}
	if _, err := session.BeginFile(context.Background(), wrongParent); !errors.Is(err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("mismatched parent admission error = %v", err)
	}
	// Simulate a process restart by dropping only the process-local overlay. The
	// durable FileCheckpointV1 is reloaded as the sole authority for continuation.
	session.mu.Lock()
	savedFile := session.files[file.Path]
	delete(session.files, file.Path)
	session.mu.Unlock()
	session.inner.stateInstall.Lock()
	savedCheckpoints := session.inner.incrementalCheckpoints
	session.inner.incrementalCheckpoints = nil
	session.inner.stateInstall.Unlock()
	restarted, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("durable checkpoint restart recovery error = %v", err)
	}
	restartedValue, _, ok := restarted.Transaction()
	if !ok {
		t.Fatalf("restart BeginFile returned no transaction: %+v", restarted)
	}
	restartedTransaction, ok := restartedValue.(*FileTransaction)
	if !ok {
		t.Fatalf("restart transaction type = %T", restartedValue)
	}
	if _, err := restartedTransaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatalf("pause restarted transaction: %v", err)
	}
	session.mu.Lock()
	session.files[file.Path] = savedFile
	session.mu.Unlock()
	session.inner.stateInstall.Lock()
	session.inner.incrementalCheckpoints = savedCheckpoints
	session.inner.stateInstall.Unlock()
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: root, Generation: generation,
	}); err != nil {
		t.Fatalf("finalize directory after file settlement: %v", err)
	}
	if _, err := session.CompleteJob(context.Background(), transfer.JobCompletedWithErrors); err != nil {
		t.Fatalf("complete incremental job: %v", err)
	}
	if len(session.inner.incrementalCheckpoints) != 1 {
		t.Fatal("completed job discarded the file checkpoint overlay")
	}
}

func firstIncrementalCheckpoint(session *Session) resumestate.FileCheckpointV1 {
	for _, checkpoint := range session.incrementalCheckpoints {
		return checkpoint
	}
	return resumestate.FileCheckpointV1{}
}

func TestIncrementalCheckpointReopensAcrossFreshAuthority(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	newIntent := func() transfer.TransferIntent {
		share := incrementalTestIdentity16[catalog.ShareInstance](0x71)
		root := incrementalTestIdentity16[catalog.DirectoryID](0x72)
		rules, err := transfer.NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := transfer.NewFilesystemTransferIntent(
			share, root, rules, rootSpec.path, filesystemOutputBackendID, transfer.OutputNativeTree,
		)
		if err != nil {
			t.Fatal(err)
		}
		return intent
	}
	intent := newIntent()
	authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	opened, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := opened.(*incrementalOutputSession)
	if !ok {
		t.Fatalf("OpenOutput returned %T, want incremental session", opened)
	}

	rootID := intent.SyntheticRoot()
	rootGeneration := incrementalTestIdentity16[catalog.DirectoryGeneration](0x73)
	rootAdmission, err := first.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: rootID, Generation: rootGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID := incrementalTestIdentity16[catalog.FileID](0x74)
	revision := incrementalTestIdentity16[content.FileRevision](0x75)
	geometry, err := content.NewFileGeometry(4, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID, revision, geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := outputTargetForDescriptor(first.SessionID(), descriptor, "reopen.bin")
	if err != nil {
		t.Fatal(err)
	}
	file := transfer.OutputFile{
		Path: "reopen.bin", ExpectedSize: 4, Descriptor: descriptor, Target: target,
		ParentAdmission: rootAdmission,
	}
	start, err := first.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transactionValue, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("initial BeginFile returned no transaction: %+v", start)
	}
	transaction := transactionValue.(*FileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopenedAuthority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	reopenedValue, err := reopenedAuthority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatalf("fresh OpenOutput: %v", err)
	}
	reopened, ok := reopenedValue.(*incrementalOutputSession)
	if !ok {
		t.Fatalf("fresh OpenOutput returned %T, want incremental session", reopenedValue)
	}
	reopenedRootAdmission, err := reopened.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		// A later catalog generation must not invalidate file-local progress. The
		// durable checkpoint is bound to the transfer intent and file revision,
		// while directory generations describe only the current traversal view.
		DirectoryID: rootID, Generation: incrementalTestIdentity16[catalog.DirectoryGeneration](0x76),
	})
	if err != nil {
		t.Fatalf("fresh root admission: %v", err)
	}
	reopenedFile := file
	reopenedFile.ParentAdmission = reopenedRootAdmission
	reopenedFile.Target, err = outputTargetForDescriptor(reopened.SessionID(), descriptor, reopenedFile.Path)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := reopened.BeginFile(context.Background(), reopenedFile)
	if err != nil {
		t.Fatalf("fresh BeginFile from durable checkpoint: %v", err)
	}
	resumedValue, _, ok := resumed.Transaction()
	if !ok {
		t.Fatalf("fresh BeginFile returned no transaction: %+v", resumed)
	}
	resumedTransaction := resumedValue.(*FileTransaction)
	ranges := resumedTransaction.resumable.BoundState().State().DurableRanges().Ranges()
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != 4 {
		t.Fatalf("fresh durable ranges = %+v, want [0,4)", ranges)
	}
	if _, err := resumedTransaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalEmptyFileCheckpointSurvivesPauseAndPublishes(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x81, 0x82)
	parent := currentCoverageRoot(t, session, intent, 0x83)
	file := currentCoverageFile(t, session, intent, "empty.bin", parent, 0x84, 0x85, 0)
	transaction := currentCoverageTransaction(t, session, file)

	checkpoint := firstIncrementalCheckpoint(session.inner)
	if checkpoint.CommitState() != resumestate.FileCheckpointCommitVerified ||
		checkpoint.CheckpointGeneration() != 0 || len(checkpoint.VerifiedRanges()) != 0 {
		t.Fatalf("initial empty checkpoint = state:%v generation:%d ranges:%+v",
			checkpoint.CommitState(), checkpoint.CheckpointGeneration(), checkpoint.VerifiedRanges())
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal("pause empty file: ", err)
	}
	if _, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal("pause empty job: ", err)
	}

	reopenedAuthority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	opened, err := reopenedAuthority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal("reopen empty checkpoint: ", err)
	}
	reopened := opened.(*incrementalOutputSession)
	reopenedParent := currentCoverageRoot(t, reopened, intent, 0x86)
	reopenedFile := currentCoverageFile(t, reopened, intent, file.Path, reopenedParent, 0x84, 0x85, 0)
	reopenedTransaction := currentCoverageTransaction(t, reopened, reopenedFile)
	settlement, err := reopenedTransaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish reopened empty file = %v, %v", settlement.Kind(), err)
	}
	published := firstIncrementalCheckpoint(reopened.inner)
	if published.CommitState() != resumestate.FileCheckpointCommitPublished ||
		published.Phase() != resumestate.FileCheckpointPhasePublished ||
		published.CheckpointGeneration() != 0 || len(published.VerifiedRanges()) != 0 {
		t.Fatalf("published empty checkpoint = state:%v phase:%v generation:%d ranges:%+v",
			published.CommitState(), published.Phase(), published.CheckpointGeneration(), published.VerifiedRanges())
	}
	info, err := os.Stat(filepath.Join(rootSpec.path, file.Path))
	if err != nil || info.Size() != 0 {
		t.Fatalf("published empty artifact = size:%d err:%v", func() int64 {
			if info == nil {
				return -1
			}
			return info.Size()
		}(), err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal("close reopened empty job: ", err)
	}
}

func TestIncrementalNonEmptyFileCanPauseBeforeFirstWriteAndResume(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x87, 0x88)
	parent := currentCoverageRoot(t, session, intent, 0x89)
	file := currentCoverageFile(t, session, intent, "unwritten.bin", parent, 0x8a, 0x8b, 4)
	transaction := currentCoverageTransaction(t, session, file)

	baseline := firstIncrementalCheckpoint(session.inner)
	if baseline.CommitState() != resumestate.FileCheckpointCommitVerified ||
		baseline.CheckpointGeneration() != 0 || len(baseline.VerifiedRanges()) != 0 {
		t.Fatalf("initial non-empty checkpoint = state:%v generation:%d ranges:%+v",
			baseline.CommitState(), baseline.CheckpointGeneration(), baseline.VerifiedRanges())
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal("pause before first write: ", err)
	}
	if _, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal("pause unwritten job: ", err)
	}

	reopenedAuthority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	opened, err := reopenedAuthority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal("reopen unwritten checkpoint: ", err)
	}
	reopened := opened.(*incrementalOutputSession)
	reopenedParent := currentCoverageRoot(t, reopened, intent, 0x8c)
	reopenedFile := currentCoverageFile(t, reopened, intent, file.Path, reopenedParent, 0x8a, 0x8b, 4)
	reopenedTransaction := currentCoverageTransaction(t, reopened, reopenedFile)
	if ranges := reopenedTransaction.resumable.BoundState().State().DurableRanges().Ranges(); len(ranges) != 0 {
		t.Fatalf("unwritten checkpoint resumed non-empty ranges: %+v", ranges)
	}
	if err := reopenedTransaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopenedTransaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	settlement, err := reopenedTransaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish resumed unwritten file = %v, %v", settlement.Kind(), err)
	}
	if _, err := reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}
