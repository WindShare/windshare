//go:build windows || linux || darwin

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	retirementRaceIterations = 16
	retirementWaitTimeout    = 5 * time.Second
	retirementFileName       = "resume.bin"
	retirementFirstHalf      = "part"
	retirementSecondHalf     = "done"
	retirementFullPayload    = retirementFirstHalf + retirementSecondHalf
)

func TestDiscardOperationAcquiresDirLockBeforeProbingTaskLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sel, intent, fileID := createPausedSingleFileOp(t, root, 0x71)
	opID := intent.OperationID()

	rootDir, _, err := OpenContainer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rootDir.Close()
	dirLock, err := acquireDirLock(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir, err := openChildDir(rootDir, ControlDirName)
	if err != nil {
		_ = dirLock.Close()
		t.Fatal(err)
	}
	defer ctrlDir.Close()

	type discardOutcome struct {
		summary RecoverySummary
		err     error
	}
	done := make(chan discardOutcome, 1)
	go func() {
		summary, discardErr := DiscardOperation(ctx, root, opID)
		done <- discardOutcome{summary: summary, err: discardErr}
	}()

	// Simulate a concurrent get/resume retry winning dirLock and acquiring
	// task.lock before releasing dirLock.
	opDirName := hex.EncodeToString(opID.Bytes())
	opDir, err := openChildDir(ctrlDir, opDirName)
	if err != nil {
		_ = dirLock.Close()
		t.Fatal(err)
	}
	taskLock, _, err := acquireLockFile(opDir, taskLockFileName, false)
	if err != nil {
		_ = opDir.Close()
		_ = dirLock.Close()
		t.Fatalf("acquire task lock while holding dirLock: %v", err)
	}
	select {
	case out := <-done:
		_ = errors.Join(taskLock.Close(), opDir.Close(), dirLock.Close())
		t.Fatalf("DiscardOperation returned before dirLock was released: (%+v, %v)", out.summary, out.err)
	default:
	}

	if err := dirLock.Close(); err != nil {
		_ = errors.Join(taskLock.Close(), opDir.Close())
		t.Fatal(err)
	}

	select {
	case out := <-done:
		if !errors.Is(out.err, ErrBusy) {
			_ = errors.Join(taskLock.Close(), opDir.Close())
			t.Fatalf("DiscardOperation error = %v, want ErrBusy", out.err)
		}
	case <-time.After(retirementWaitTimeout):
		_ = errors.Join(taskLock.Close(), opDir.Close())
		t.Fatal("timed out waiting for DiscardOperation")
	}

	if err := errors.Join(taskLock.Close(), opDir.Close()); err != nil {
		t.Fatal(err)
	}
	finishPausedSingleFileOp(t, root, sel, receivecontract.AutomaticOperation(), fileID, 0x71)
}

func TestDiscardOperationAndConcurrentReopenNeverCorruptActiveResume(t *testing.T) {
	ctx := context.Background()
	for i := range retirementRaceIterations {
		root := t.TempDir()
		seed := byte(0x80 + i)
		sel, intent, fileID := createPausedSingleFileOp(t, root, seed)
		opID := intent.OperationID()

		selector := receivecontract.AutomaticOperation()
		if i%2 == 1 {
			var err error
			selector, err = receivecontract.SelectOperationID(opID)
			if err != nil {
				t.Fatal(err)
			}
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := reopen.BindDestination(ctx); err != nil {
			_ = reopen.Close()
			t.Fatal(err)
		}

		start := make(chan struct{})
		var (
			wg            sync.WaitGroup
			discardRes    RecoverySummary
			discardErr    error
			lookup        ActiveLookup
			lookupErr     error
			reopenDoneErr error
		)

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			discardRes, discardErr = DiscardOperation(ctx, root, opID)
		}()
		go func() {
			defer wg.Done()
			<-start
			lookup, lookupErr = reopen.SelectOperation(ctx, sel, selector)
			if lookupErr == nil && lookup.Kind() == LookupReopened {
				reopenDoneErr = completeReopenedSingleFile(ctx, reopen, lookup.Operation(), fileID, seed)
			}
		}()

		close(start)
		wg.Wait()
		closeErr := reopen.Close()
		if lookupErr != nil || closeErr != nil {
			t.Fatalf("iter %d: lookupErr=%v closeErr=%v", i, lookupErr, closeErr)
		}

		switch lookup.Kind() {
		case LookupReopened:
			// completeReopenedSingleFile retires the operation via FinalizeTree(Success),
			// so a concurrent DiscardOperation either observes the active task.lock
			// (ErrBusy) or acquires dirLock after completion already removed opDir
			// (FailureLookup with fs.ErrNotExist). It must never discard or corrupt it.
			diag, hasDiag := DiagnosticFor(discardErr)
			retiredBeforeDiscard := hasDiag && diag.Stage == FailureLookup && errors.Is(discardErr, fs.ErrNotExist)
			if discardRes.Valid() || FailureForStage(discardErr, FailureClose) != nil ||
				(!errors.Is(discardErr, ErrBusy) && !retiredBeforeDiscard) {
				t.Fatalf(
					"iter %d: reopened op raced with discard = (%+v, %v), want ErrBusy or not-found after retirement",
					i, discardRes, discardErr,
				)
			}
			if reopenDoneErr != nil {
				t.Fatalf("iter %d: reopened download failed after concurrent discard: %v", i, reopenDoneErr)
			}
			got, err := os.ReadFile(filepath.Join(root, retirementFileName))
			if err != nil || string(got) != retirementFullPayload {
				t.Fatalf("iter %d: published file = (%q, %v), want %q", i, got, err, retirementFullPayload)
			}
		case LookupCreateReady, LookupNotFound:
			if discardErr != nil || discardRes.State() != RecoveryOperationDiscarded {
				t.Fatalf("iter %d: discard = (%+v, %v), want RecoveryOperationDiscarded", i, discardRes, discardErr)
			}
		default:
			t.Fatalf("iter %d: unexpected lookup kind %d (discard=%+v, err=%v)", i, lookup.Kind(), discardRes, discardErr)
		}
	}
}

func TestOperationDiscardKeepsTaskLockUntilDirLockHeld(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xa1)

	dirLock, ctrlDir, opDir := holdDirLockAndOpenOpDir(t, root, intent.OperationID())
	defer ctrlDir.Close()
	defer opDir.Close()

	done := make(chan error, 1)
	go func() {
		done <- op.Discard(ctx)
	}()

	// Discard transitions Authority to closed before blocking on dirLock.
	waitUntil(t, func() bool { return !op.Idle() })

	probe, _, probeErr := acquireLockFile(opDir, taskLockFileName, true)
	if !errors.Is(probeErr, ErrNamespaceLockBusy) {
		if probe != nil {
			_ = probe.Close()
		}
		_ = dirLock.Close()
		t.Fatalf("task.lock was released before dirLock acquisition: probeErr=%v", probeErr)
	}
	if err := errors.Join(opDir.Close(), ctrlDir.Close(), dirLock.Close()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Operation.Discard = %v", err)
		}
	case <-time.After(retirementWaitTimeout):
		t.Fatal("timed out waiting for Operation.Discard")
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("Authority.Close after Discard = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived Operation.Discard: %v", err)
	}
}

func TestFinalizeTreeSuccessKeepsTaskLockUntilDirLockHeld(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xb1)
	defer auth.Close()

	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	fileID := testID16[catalog.FileID](0xb3)
	f := testSingleFileMaterialization(t, session, intent, fileID, 0xb4, retirementFileName, uint64(len(retirementFullPayload)))
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte(retirementFullPayload)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	dirLock, ctrlDir, opDir := holdDirLockAndOpenOpDir(t, root, intent.OperationID())
	defer ctrlDir.Close()
	defer opDir.Close()

	done := make(chan error, 1)
	go func() {
		settlement, finErr := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
		if finErr == nil && settlement.Kind() != transfer.DirectTreeSettlementSuccess {
			finErr = errors.New("unexpected tree settlement")
		}
		done <- finErr
	}()

	waitUntil(t, func() bool {
		auth.mu.Lock()
		defer auth.mu.Unlock()
		return op.completed
	})

	probe, _, probeErr := acquireLockFile(opDir, taskLockFileName, true)
	if !errors.Is(probeErr, ErrNamespaceLockBusy) {
		if probe != nil {
			_ = probe.Close()
		}
		_ = dirLock.Close()
		t.Fatalf("task.lock was released before FinalizeTree acquired dirLock: probeErr=%v", probeErr)
	}
	if err := errors.Join(opDir.Close(), ctrlDir.Close(), dirLock.Close()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FinalizeTree(Success) = %v", err)
		}
	case <-time.After(retirementWaitTimeout):
		t.Fatal("timed out waiting for FinalizeTree")
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("Authority.Close after FinalizeTree = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived FinalizeTree: %v", err)
	}
}

func TestDiscardOperationRemovesCorruptJournalUnderDirLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0xc1)
	opID := intent.OperationID()

	journalPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()), journalFileName)
	if err := os.WriteFile(journalPath, []byte("not-a-valid-journal\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := DiscardOperation(ctx, root, opID)
	if err != nil || summary.State() != RecoveryOperationDiscarded || summary.OperationID() != opID {
		t.Fatalf("DiscardOperation on corrupt journal = (%+v, %v)", summary, err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived corrupt-journal discard: %v", err)
	}
}

func TestDiscardOperationRejectsReplacedOperationIdentityUnderDirLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, originalIntent, _ := createPausedSingleFileOp(t, root, 0xc2)
	_, replacementIntent, _ := createPausedSingleFileOp(t, root, 0xc6)
	originalID := originalIntent.OperationID()
	replacementID := replacementIntent.OperationID()

	originalDir := filepath.Join(root, ControlDirName, hex.EncodeToString(originalID.Bytes()))
	replacementDir := filepath.Join(root, ControlDirName, hex.EncodeToString(replacementID.Bytes()))
	if err := os.RemoveAll(originalDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementDir, originalDir); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(originalDir, taskLockFileName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(originalDir, journalFileName)
	wantJournal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	summary, err := DiscardOperation(ctx, root, originalID)
	if !errors.Is(err, ErrOperationChanged) || summary.Valid() {
		t.Fatalf("DiscardOperation on replaced operation = (%+v, %v), want ErrOperationChanged", summary, err)
	}
	gotJournal, err := os.ReadFile(journalPath)
	if err != nil || string(gotJournal) != string(wantJournal) {
		t.Fatalf("replaced journal changed after refused discard: len=%d err=%v", len(gotJournal), err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused discard created task.lock in replaced dir: %v", err)
	}
	entries, err := os.ReadDir(originalDir)
	if err != nil {
		t.Fatal(err)
	}
	hasPart := false
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".part" {
			hasPart = true
			break
		}
	}
	if !hasPart {
		t.Fatalf("replaced operation .part file was lost: entries=%v", entries)
	}
}

func TestOperationRetirementRemovesPrivateScratchAndPreservesExternalEntries(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"discard", "complete"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			seed := byte(0xc3)
			if mode == "complete" {
				seed = 0xc4
			}
			sel, intent, fileID := createPausedSingleFileOp(t, root, seed)
			opID := intent.OperationID()

			destForeign := filepath.Join(root, "user-note.txt")
			if err := os.WriteFile(destForeign, []byte("keep-dest"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctrlSibling := filepath.Join(root, ControlDirName, "unrelated.txt")
			if err := os.WriteFile(ctrlSibling, []byte("keep-ctrl"), 0o600); err != nil {
				t.Fatal(err)
			}
			opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
			scratchExtra := filepath.Join(opDir, "state.log (conflicted copy)")
			if err := os.WriteFile(scratchExtra, []byte("scratch-junk"), 0o600); err != nil {
				t.Fatal(err)
			}

			if mode == "discard" {
				summary, err := DiscardOperation(ctx, root, opID)
				if err != nil || summary.State() != RecoveryOperationDiscarded {
					t.Fatalf("DiscardOperation = (%+v, %v)", summary, err)
				}
			} else {
				finishPausedSingleFileOp(t, root, sel, receivecontract.AutomaticOperation(), fileID, seed)
			}

			if _, err := os.Stat(opDir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("operation scratch dir survived %s: %v", mode, err)
			}
			if got, err := os.ReadFile(destForeign); err != nil || string(got) != "keep-dest" {
				t.Fatalf("destination file after %s = (%q, %v)", mode, got, err)
			}
			if got, err := os.ReadFile(ctrlSibling); err != nil || string(got) != "keep-ctrl" {
				t.Fatalf("unrelated control-dir sibling after %s = (%q, %v)", mode, got, err)
			}
		})
	}
}

func openActiveSingleFileOp(t *testing.T, root string, seed byte) (*Authority, *Operation, transfer.ReceiveIntent) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	fileID := testID16[catalog.FileID](seed + 2)
	art, err := receivecontract.NewSingleFileDirectoryTree(fileID, retirementFileName, retirementFileName)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	return auth, op, intent
}

func createPausedSingleFileOp(t *testing.T, root string, seed byte) (transfer.SelectionSpec, transfer.ReceiveIntent, catalog.FileID) {
	t.Helper()
	ctx := context.Background()
	auth, op, intent := openActiveSingleFileOp(t, root, seed)
	fileID := testID16[catalog.FileID](seed + 2)
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	f := testSingleFileMaterialization(t, session, intent, fileID, seed+3, retirementFileName, uint64(len(retirementFullPayload)))
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		_ = auth.Close()
		t.Fatal("expected initial file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte(retirementFirstHalf)); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	return intent.SelectionSpec(), intent, fileID
}

func finishPausedSingleFileOp(
	t *testing.T,
	root string,
	sel transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
	fileID catalog.FileID,
	seed byte,
) {
	t.Helper()
	ctx := context.Background()
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := reopen.SelectOperation(ctx, sel, selector)
	if err != nil || lookup.Kind() != LookupReopened {
		t.Fatalf("reopen select = (%v, %v)", lookup.Kind(), err)
	}
	if err := completeReopenedSingleFile(ctx, reopen, lookup.Operation(), fileID, seed); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, retirementFileName))
	if err != nil || string(got) != retirementFullPayload {
		t.Fatalf("published content = (%q, %v), want %q", got, err, retirementFullPayload)
	}
}

func completeReopenedSingleFile(
	ctx context.Context,
	auth *Authority,
	op *Operation,
	fileID catalog.FileID,
	seed byte,
) error {
	intent, ok := op.ReceiveIntent()
	if !ok {
		return errors.New("reopened operation has no intent")
	}
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		return err
	}
	f, err := buildSingleFileMaterialization(
		session, intent, fileID, seed+3, retirementFileName, uint64(len(retirementFullPayload)),
	)
	if err != nil {
		return err
	}
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		return err
	}
	tx, durable, ok := start.Transaction()
	if !ok {
		return errors.New("reopened file did not start transaction")
	}
	ranges := durable.Ranges().Ranges()
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != uint64(len(retirementFirstHalf)) {
		return errors.New("reopened file lost persisted partial range")
	}
	if err := tx.WriteRange(ctx, uint64(len(retirementFirstHalf)), []byte(retirementSecondHalf)); err != nil {
		return err
	}
	if _, err := tx.Commit(ctx); err != nil {
		return err
	}
	_, err = session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
	return err
}

func TestActiveOperationCreatesNoControlDirLockFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xc5)

	entries, err := os.ReadDir(filepath.Join(root, ControlDirName))
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	wantOpDir := hex.EncodeToString(intent.OperationID().Bytes())
	for _, entry := range entries {
		if entry.Name() != authorityIDFile && entry.Name() != wantOpDir {
			_ = auth.Close()
			t.Fatalf("unexpected entry inside %s: %q", ControlDirName, entry.Name())
		}
	}
	if err := op.Discard(ctx); err != nil {
		_ = auth.Close()
		t.Fatalf("Operation.Discard = %v", err)
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("Authority.Close = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived discard: %v", err)
	}
}

func TestLastTaskRetirementSerializesConcurrentAdmissions(t *testing.T) {
	ctx := context.Background()
	for i := range retirementRaceIterations {
		root := t.TempDir()
		retireSeed := byte(0xd0 + i)
		_, retireIntent, _ := createPausedSingleFileOp(t, root, retireSeed)
		retireOpID := retireIntent.OperationID()

		newSeed := byte(0xe0 + i)
		sel := testSelectionSpec(t, newSeed)
		fileID := testID16[catalog.FileID](newSeed + 2)
		art, err := receivecontract.NewSingleFileDirectoryTree(fileID, "concurrent.bin", "concurrent.bin")
		if err != nil {
			t.Fatal(err)
		}

		selector := receivecontract.AutomaticOperation()
		redownload := i%2 == 1
		if redownload {
			selector = receivecontract.RedownloadOperation()
		}
		authA, lookupA := prepareCreateReadyAuthority(t, root, sel, selector)
		authB, lookupB := prepareCreateReadyAuthority(t, root, sel, selector)

		start := make(chan struct{})
		var (
			wg         sync.WaitGroup
			discardRes RecoverySummary
			discardErr error
			opA, opB   *Operation
			errA, errB error
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			discardRes, discardErr = DiscardOperation(ctx, root, retireOpID)
		}()
		go func() {
			defer wg.Done()
			<-start
			opA, errA = authA.CreateOperation(ctx, lookupA, art)
		}()
		go func() {
			defer wg.Done()
			<-start
			opB, errB = authB.CreateOperation(ctx, lookupB, art)
		}()

		close(start)
		wg.Wait()
		if discardErr != nil || discardRes.State() != RecoveryOperationDiscarded {
			_ = errors.Join(authA.Close(), authB.Close())
			t.Fatalf("iter %d: discard = (%+v, %v), want discarded", i, discardRes, discardErr)
		}

		if redownload {
			if errA != nil || errB != nil || opA == nil || opB == nil {
				_ = errors.Join(authA.Close(), authB.Close())
				t.Fatalf("iter %d: redownload create errors = (%v, %v)", i, errA, errB)
			}
			resA := mustPhysicalReservationName(t, opA)
			resB := mustPhysicalReservationName(t, opB)
			if resA == resB {
				_ = errors.Join(authA.Close(), authB.Close())
				t.Fatalf("iter %d: concurrent redownload creators reserved the same physical name %q", i, resA)
			}
		} else {
			aCreated := errA == nil && opA != nil
			bCreated := errB == nil && opB != nil
			if aCreated == bCreated {
				_ = errors.Join(authA.Close(), authB.Close())
				t.Fatalf("iter %d: expected exactly one automatic creator, got (opA=%v errA=%v) (opB=%v errB=%v)", i, opA != nil, errA, opB != nil, errB)
			}
			loserErr := errB
			if !aCreated {
				loserErr = errA
			}
			if !errors.Is(loserErr, ErrAlreadyRunning) {
				_ = errors.Join(authA.Close(), authB.Close())
				t.Fatalf("iter %d: losing automatic creator error = %v, want ErrAlreadyRunning", i, loserErr)
			}
		}

		if err := errors.Join(authA.Close(), authB.Close()); err != nil {
			t.Fatalf("iter %d: close authorities = %v", i, err)
		}
	}
}

func prepareCreateReadyAuthority(
	t *testing.T,
	root string,
	sel transfer.SelectionSpec,
	selector receivecontract.OperationSelection,
) (*Authority, ActiveLookup) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	lookup, err := auth.SelectOperation(ctx, sel, selector)
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("SelectOperation = (%v, %v), want LookupCreateReady", lookup.Kind(), err)
	}
	return auth, lookup
}

func mustPhysicalReservationName(t *testing.T, op *Operation) string {
	t.Helper()
	intent, ok := op.ReceiveIntent()
	if !ok {
		t.Fatal("operation missing receive intent")
	}
	res, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		t.Fatal("operation missing destination reservation")
	}
	return res.PhysicalName()
}

func holdDirLockAndOpenOpDir(t *testing.T, root string, opID receivecontract.OperationID) (*DirLock, *Dir, *Dir) {
	t.Helper()
	rootDir, _, err := OpenContainer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rootDir.Close()
	dirLock, err := acquireDirLock(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir, err := openChildDir(rootDir, ControlDirName)
	if err != nil {
		_ = dirLock.Close()
		t.Fatal(err)
	}
	opDir, err := openChildDir(ctrlDir, hex.EncodeToString(opID.Bytes()))
	if err != nil {
		_ = errors.Join(ctrlDir.Close(), dirLock.Close())
		t.Fatal(err)
	}
	return dirLock, ctrlDir, opDir
}

func buildSingleFileMaterialization(
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	fileID catalog.FileID,
	revSeed byte,
	name string,
	size uint64,
) (transfer.MaterializationFile, error) {
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		return transfer.MaterializationFile{}, err
	}
	desc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID, testID16[content.FileRevision](revSeed), geom, catalog.ModifiedTime{},
	)
	if err != nil {
		return transfer.MaterializationFile{}, err
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		return transfer.MaterializationFile{}, err
	}
	fileParent, err := transfer.NewReferenceMaterializationFileParent(
		intent.SyntheticRoot(), testID16[catalog.DirectoryGeneration](revSeed+1), ordinaryoutput.EmptySourceCatalogPath(),
	)
	if err != nil {
		return transfer.MaterializationFile{}, err
	}
	rel, err := transfer.NewMaterializationRootRelativePath(name)
	if err != nil {
		return transfer.MaterializationFile{}, err
	}
	return transfer.NewMaterializationFile(intent, sourcePath, rel, desc, session.SessionID(), fileParent)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(retirementWaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for expected lock state transition")
		}
		runtime.Gosched()
	}
}
