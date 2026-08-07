package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCurrentIncrementalSessionRejectsInvalidAndTerminalCalls(t *testing.T) {
	var nilSession *incrementalOutputSession
	if nilSession.BackendID() != "" || nilSession.SessionID() != (transfer.OutputSessionID{}) ||
		nilSession.Capabilities() != (transfer.OutputCapabilities{}) {
		t.Fatal("nil incremental session exposed authority")
	}
	if _, err := nilSession.AdmitDirectory(context.Background(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("nil session admitted a directory")
	}
	if err := nilSession.FinalizeDirectory(context.Background(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("nil session finalized a directory")
	}
	if _, err := nilSession.BeginFile(context.Background(), transfer.OutputFile{}); err == nil {
		t.Fatal("nil session began a file")
	}
	if _, err := nilSession.PauseJob(context.Background(), transfer.JobPauseInterrupted); err == nil {
		t.Fatal("nil session paused")
	}
	if _, err := nilSession.CompleteJob(context.Background(), transfer.JobSucceeded); err == nil {
		t.Fatal("nil session completed")
	}

	rootSpec := newRuntimeTestRootSpec(t)
	session, _ := currentCoverageSession(t, rootSpec, 0x10, 0x11)
	if session.BackendID() == "" || session.SessionID() == (transfer.OutputSessionID{}) ||
		session.Capabilities() == (transfer.OutputCapabilities{}) {
		t.Fatal("live incremental session omitted authority metadata")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.AdmitDirectory(context.TODO(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("empty directory was admitted")
	}
	if _, err := session.AdmitDirectory(canceled, transfer.OutputDirectory{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory admission = %v", err)
	}
	if err := session.FinalizeDirectory(context.TODO(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("empty directory was finalized")
	}
	if err := session.FinalizeDirectory(canceled, transfer.OutputDirectory{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory finalization = %v", err)
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("unadmitted directory was finalized")
	}
	if _, err := session.BeginFile(context.TODO(), transfer.OutputFile{}); err == nil {
		t.Fatal("empty file was begun")
	}
	if _, err := session.BeginFile(canceled, transfer.OutputFile{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled BeginFile = %v", err)
	}
	if _, err := session.BeginFile(context.Background(), transfer.OutputFile{Path: "../escape"}); err == nil {
		t.Fatal("non-canonical path began a file")
	}
	if _, err := session.BeginFile(context.Background(), transfer.OutputFile{Path: "before-root.bin"}); !errors.Is(err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("file before root admission = %v", err)
	}
	if _, err := session.CompleteJob(context.Background(), transfer.JobPausedOutcome); err == nil {
		t.Fatal("paused outcome completed an output session")
	}
	paused, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil || paused.Kind() != transfer.JobPaused {
		t.Fatalf("pause before root admission = (%d, %v)", paused.Kind(), err)
	}
	paused, err = session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil || paused.Kind() != transfer.JobPaused {
		t.Fatalf("repeated pause = (%d, %v)", paused.Kind(), err)
	}
	closed, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || closed.Kind() != transfer.JobClosed {
		t.Fatalf("complete after pause = (%d, %v)", closed.Kind(), err)
	}
	if _, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("closed session admitted a directory")
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{}); err == nil {
		t.Fatal("closed session finalized a directory")
	}
	if _, err := session.BeginFile(context.Background(), transfer.OutputFile{Path: "closed.bin"}); err == nil {
		t.Fatal("closed session began a file")
	}

	// A wrapper without a retained platform can only arise while unwinding a
	// failed open. Its terminal methods must still be total and idempotent.
	failedOpen := &incrementalOutputSession{}
	if settlement, err := failedOpen.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil || settlement.Kind() != transfer.JobPaused {
		t.Fatalf("failed-open pause = (%d, %v)", settlement.Kind(), err)
	}
	failedOpen = &incrementalOutputSession{}
	if settlement, err := failedOpen.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil || settlement.Kind() != transfer.JobClosed {
		t.Fatalf("failed-open completion = (%d, %v)", settlement.Kind(), err)
	}
}

func TestCurrentFileTransactionEnforcesEveryPublicBoundary(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x12, 0x13)
	rootGeneration := incrementalTestIdentity16[catalog.DirectoryGeneration](0x14)
	parent := currentCoverageRoot(t, session, intent, 0x14)
	file := currentCoverageFile(t, session, intent, "boundary.bin", parent, 0x15, 0x16, 4)

	wrongParent := file
	wrongParent.ParentAdmission = transfer.DirectoryAdmission{}
	if _, err := session.BeginFile(context.Background(), wrongParent); err == nil {
		t.Fatal("file with foreign parent admission began")
	}
	wrongSize := file
	wrongSize.ExpectedSize++
	if _, err := session.BeginFile(context.Background(), wrongSize); err == nil {
		t.Fatal("file with inconsistent size began")
	}
	wrongTarget := file
	wrongTarget.Target = transfer.OutputFileTarget{}
	if _, err := session.BeginFile(context.Background(), wrongTarget); err == nil {
		t.Fatal("file with foreign target began")
	}

	transaction := currentCoverageTransaction(t, session, file)
	if _, err := session.BeginFile(context.Background(), file); err == nil {
		t.Fatal("active file began twice")
	}
	revised := currentCoverageFile(t, session, intent, file.Path, parent, 0x15, 0x17, 4)
	if _, err := session.BeginFile(context.Background(), revised); err == nil {
		t.Fatal("path changed revision while its immutable ledger was live")
	}
	rootDirectory := transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  rootGeneration,
	}
	if err := session.FinalizeDirectory(context.Background(), rootDirectory); err == nil {
		t.Fatal("directory finalized while a child file was active")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var nilTransaction *FileTransaction
	if err := nilTransaction.WriteRange(context.Background(), 0, nil); err == nil {
		t.Fatal("nil transaction accepted a write")
	}
	if _, err := nilTransaction.Checkpoint(context.Background()); err == nil {
		t.Fatal("nil transaction checkpointed")
	}
	if _, err := nilTransaction.Commit(context.Background()); err == nil {
		t.Fatal("nil transaction committed")
	}
	if _, err := nilTransaction.Pause(context.Background(), transfer.FilePauseInterrupted); err == nil {
		t.Fatal("nil transaction paused")
	}
	if _, err := nilTransaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err == nil {
		t.Fatal("nil transaction retired")
	}
	if err := transaction.WriteRange(canceled, 0, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	if _, err := transaction.Checkpoint(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled checkpoint = %v", err)
	}
	if _, err := transaction.Commit(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit = %v", err)
	}
	if _, err := transaction.Retire(canceled, transfer.FileRetireExplicitPolicySkip); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retirement = %v", err)
	}
	if _, err := transaction.Pause(context.Background(), 0); err == nil {
		t.Fatal("invalid pause reason settled a transaction")
	}
	if _, err := transaction.Retire(context.Background(), 0); err == nil {
		t.Fatal("invalid retirement reason settled a transaction")
	}
	if err := transaction.WriteRange(context.Background(), 0, nil); err != nil {
		t.Fatalf("empty write = %v", err)
	}
	if err := transaction.WriteRange(context.Background(), 4, []byte("x")); !errors.Is(err, outputfault.ErrOutOfRange) {
		t.Fatalf("out-of-range write = %v", err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 1, []byte("b")); err == nil {
		t.Fatal("pending range was overwritten")
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatalf("empty checkpoint = %v", err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("a")); err == nil {
		t.Fatal("durably checkpointed range was overwritten")
	}

	settlement, err := transaction.pauseForBeginFileCleanup(
		context.Background(), transfer.FilePauseOutputFailure,
	)
	if err != nil || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("BeginFile cleanup pause = (%d, %v)", settlement.Kind(), err)
	}
	for name, operation := range map[string]func() error{
		"write":      func() error { return transaction.WriteRange(context.Background(), 2, []byte("x")) },
		"checkpoint": func() error { _, err := transaction.Checkpoint(context.Background()); return err },
		"commit":     func() error { _, err := transaction.Commit(context.Background()); return err },
		"pause": func() error {
			_, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
			return err
		},
		"retire": func() error {
			_, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip)
			return err
		},
	} {
		if err := operation(); err == nil {
			t.Errorf("closed transaction accepted %s", name)
		}
	}
	if err := session.FinalizeDirectory(context.Background(), rootDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := session.BeginFile(context.Background(), file); !errors.Is(err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("finalized directory admitted file = %v", err)
	}
	if settlement, err := session.CompleteJob(context.Background(), transfer.JobCompletedWithErrors); err != nil || settlement.Kind() != transfer.JobClosed {
		t.Fatalf("terminal completion = (%d, %v)", settlement.Kind(), err)
	}
}

func TestCurrentCompletionFailsClosedWithActiveOrCanceledWork(t *testing.T) {
	for index, test := range []struct {
		name     string
		canceled bool
	}{
		{name: "active-file"},
		{name: "canceled-context", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootSpec := newRuntimeTestRootSpec(t)
			session, intent := currentCoverageSession(t, rootSpec, byte(0x30+index*4), byte(0x31+index*4))
			parent := currentCoverageRoot(t, session, intent, byte(0x32+index*4))
			if !test.canceled {
				file := currentCoverageFile(t, session, intent, "active.bin", parent, byte(0x33+index*4), byte(0x34+index*4), 1)
				_ = currentCoverageTransaction(t, session, file)
			}
			ctx := context.Background()
			if test.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := session.CompleteJob(ctx, transfer.JobSucceeded); err == nil {
				t.Fatal("completion failure did not close the uncertain owner")
			}
			if _, err := session.BeginFile(context.Background(), transfer.OutputFile{Path: "after-failure.bin"}); err == nil {
				t.Fatal("failed owner settlement left the session writable")
			}
		})
	}
}

func TestCurrentImmediateSettlementsUseDurableIdentity(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0x50, 0x51)
	parent := currentCoverageRoot(t, session, intent, 0x52)
	file := currentCoverageFile(t, session, intent, "identity.bin", parent, 0x53, 0x54, 1)
	transaction := currentCoverageTransaction(t, session, file)
	t.Cleanup(func() { _, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted) })

	if start, err := session.inner.collisionStart(file); err != nil {
		t.Fatal(err)
	} else if settlement, ok := start.ImmediateSettlement(); !ok || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("collision start = (%d, %t)", settlement.Kind(), ok)
	}
	if _, err := session.inner.collisionStart(transfer.OutputFile{}); err == nil {
		t.Fatal("collision without a target succeeded")
	}
	quarantined, err := resumestate.PrepareCheckpointRuntimeUnsafeNamespaceQuarantine(
		transaction.resumable.BoundState(), resumestate.QuarantineStageUnsafe,
	)
	if err != nil {
		t.Fatal(err)
	}
	if settlement, err := quarantinedSettlement(transaction.binding, quarantined.State()); err != nil {
		t.Fatal(err)
	} else if settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("quarantined settlement = %d", settlement.Kind())
	}
	if start, err := session.inner.quarantinedStart(
		file.Target, quarantined.State().LocatorDigest(), transfer.QuarantineOwnershipMismatch,
	); err != nil {
		t.Fatal(err)
	} else if settlement, ok := start.ImmediateSettlement(); !ok || settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("quarantine start = (%d, %t)", settlement.Kind(), ok)
	}
	if _, err := session.inner.quarantinedStart(
		transfer.OutputFileTarget{}, quarantined.State().LocatorDigest(), transfer.QuarantineOwnershipMismatch,
	); err == nil {
		t.Fatal("quarantine without a target succeeded")
	}
}
