//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func testAdmitSubdir(
	t *testing.T,
	session *Session,
	intent transfer.ReceiveIntent,
	rootAdm transfer.DirectoryAdmission,
	name string,
	seed byte,
) (transfer.DirectoryAdmission, transfer.DirectoryMaterializationRequest) {
	t.Helper()
	rootReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: rootAdm.DirectoryID(),
			Generation:  rootAdm.Generation(),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootClaim, err := transfer.NewMaterializedDirectoryClaim(rootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	subSrc, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	subReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID:     testID16[catalog.DirectoryID](seed),
			Generation:      testID16[catalog.DirectoryGeneration](seed + 1),
			SourcePath:      subSrc,
			ParentAdmission: rootAdm,
		},
		ordinaryoutput.SourceNodeSelected,
		rootClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	subAdm, err := session.AdmitDirectory(context.Background(), subReq)
	if err != nil {
		t.Fatal(err)
	}
	return subAdm, subReq
}

func testSubdirMaterializationFile(
	t *testing.T,
	session *Session,
	intent transfer.ReceiveIntent,
	subAdm transfer.DirectoryAdmission,
	subReq transfer.DirectoryMaterializationRequest,
	subdirName string,
	fileName string,
	seed byte,
	size uint64,
) transfer.MaterializationFile {
	t.Helper()
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(),
		testID16[catalog.FileID](seed),
		testID16[content.FileRevision](seed+1),
		geom,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	srcPath, err := ordinaryoutput.NewSourceCatalogPath(subdirName + "/" + fileName)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := transfer.NewMaterializedDirectoryClaim(subAdm, subReq)
	if err != nil {
		t.Fatal(err)
	}
	subSrc := subReq.Source()
	parent, err := transfer.NewDirectoryMaterializationFileParent(
		subSrc.DirectoryID, subSrc.Generation, subSrc.SourcePath,
		subAdm, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := transfer.NewMaterializationRootRelativePath(subAdm.Path() + "/" + fileName)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(intent, srcPath, rel, desc, session.SessionID(), parent)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSessionFinalizeDirectoryRejectsActiveFileUntilSettled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0x31)
	defer auth.Close()

	subAdm, subReq := testAdmitSubdir(t, session, intent, rootAdm, "nested", 0x33)
	res, _ := intent.MaterializationPlan().DestinationReservation()
	subDisk := filepath.Join(root, res.PhysicalName(), "nested")

	// 1. Active transaction blocks FinalizeDirectory without closing parent handle;
	// Commit detaches it and allows FinalizeDirectory after all files settle.
	payload := []byte("published-data")
	pubFile := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "nested", "pub.bin", 0x35, uint64(len(payload)))
	pubStart, err := session.BeginFile(ctx, pubFile)
	if err != nil {
		t.Fatal(err)
	}
	pubTx, _, ok := pubStart.Transaction()
	if !ok {
		t.Fatal("expected active transaction for pub.bin")
	}

	if _, err := session.FinalizeDirectory(ctx, subAdm); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("FinalizeDirectory with active file = %v, want ErrInvalidOutputBinding", err)
	}
	if err := pubTx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if settled, err := pubTx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("Commit after rejected FinalizeDirectory = (%v, %v)", settled.Kind(), err)
	}

	// 2. Pause, Retire, and Commit-collision must each decrement activeFiles.
	pauseFile := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "nested", "pause.bin", 0x37, 4)
	pauseStart, err := session.BeginFile(ctx, pauseFile)
	if err != nil {
		t.Fatal(err)
	}
	pauseTx, _, ok := pauseStart.Transaction()
	if !ok {
		t.Fatal("expected active transaction for pause.bin")
	}
	if _, err := session.FinalizeDirectory(ctx, subAdm); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("FinalizeDirectory with paused candidate = %v, want ErrInvalidOutputBinding", err)
	}
	if paused, err := pauseTx.Pause(ctx); err != nil || paused.Kind() != transfer.FilePaused {
		t.Fatalf("Pause = (%v, %v)", paused.Kind(), err)
	}

	retireFile := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "nested", "retire.bin", 0x39, 4)
	retireStart, err := session.BeginFile(ctx, retireFile)
	if err != nil {
		t.Fatal(err)
	}
	retireTx, _, ok := retireStart.Transaction()
	if !ok {
		t.Fatal("expected active transaction for retire.bin")
	}
	if _, err := session.FinalizeDirectory(ctx, subAdm); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("FinalizeDirectory with retire candidate = %v, want ErrInvalidOutputBinding", err)
	}
	if retired, err := retireTx.Retire(ctx); err != nil || retired.Kind() != transfer.FileFailed {
		t.Fatalf("Retire = (%v, %v)", retired.Kind(), err)
	}

	collideFile := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "nested", "collide.bin", 0x3b, 4)
	collideStart, err := session.BeginFile(ctx, collideFile)
	if err != nil {
		t.Fatal(err)
	}
	collideTx, _, ok := collideStart.Transaction()
	if !ok {
		t.Fatal("expected active transaction for collide.bin")
	}
	if err := collideTx.WriteRange(ctx, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDisk, "collide.bin"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if collided, err := collideTx.Commit(ctx); err != nil || collided.Kind() != transfer.FileCollision {
		t.Fatalf("Commit collision = (%v, %v)", collided.Kind(), err)
	}

	if settled, err := session.FinalizeDirectory(ctx, subAdm); err != nil || settled.Kind() != transfer.DirectoryFinalized {
		t.Fatalf("FinalizeDirectory after all files settled = (%v, %v)", settled.Kind(), err)
	}
	session.mu.Lock()
	subState := session.dirsByLogical[subAdm.Path()]
	activeCount := subState.activeFiles
	dirHandle := subState.dir
	session.mu.Unlock()
	if activeCount != 0 || dirHandle != nil {
		t.Fatalf("post-finalize dirState = (activeFiles=%d, dir=%v), want (0, nil)", activeCount, dirHandle)
	}

	got, err := os.ReadFile(filepath.Join(subDisk, "pub.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published file = (%q, %v), want %q", got, err, payload)
	}
}

func TestSessionConcurrentBeginFileSamePathAdmitsExactlyOne(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0x41)
	defer auth.Close()

	subAdm, subReq := testAdmitSubdir(t, session, intent, rootAdm, "shared", 0x43)
	payload := []byte("single-owner-content")
	file := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "shared", "same.bin", 0x45, uint64(len(payload)))

	const callers = 8
	start := make(chan struct{})
	type result struct {
		start transfer.FileStart
		err   error
	}
	results := make([]result, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func(idx int) {
			defer wg.Done()
			<-start
			st, err := session.BeginFile(ctx, file)
			results[idx] = result{start: st, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var winner transfer.FileTransaction
	var rejected int
	for _, r := range results {
		if r.err == nil {
			tx, _, ok := r.start.Transaction()
			if !ok {
				t.Fatalf("winning BeginFile returned non-transaction start: %+v", r.start)
			}
			if winner != nil {
				t.Fatal("multiple concurrent BeginFile calls admitted the same path")
			}
			winner = tx
			continue
		}
		if !errors.Is(r.err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("rejected concurrent BeginFile err = %v, want ErrInvalidOutputBinding", r.err)
		}
		rejected++
	}
	if winner == nil || rejected != callers-1 {
		t.Fatalf("concurrent BeginFile outcomes = (winner=%t, rejected=%d), want (true, %d)", winner != nil, rejected, callers-1)
	}

	if err := winner.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if settled, err := winner.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("winner Commit = (%v, %v)", settled.Kind(), err)
	}
	if _, err := session.FinalizeDirectory(ctx, subAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if tree, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("FinalizeTree = (%v, %v)", tree.Kind(), err)
	}

	res, _ := intent.MaterializationPlan().DestinationReservation()
	got, err := os.ReadFile(filepath.Join(root, res.PhysicalName(), "shared", "same.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published content = (%q, %v), want %q", got, err, payload)
	}
}

func TestSessionConcurrentBeginFileAndFinalizeDirectoryIsRaceFree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0x51)
	defer auth.Close()

	res, _ := intent.MaterializationPlan().DestinationReservation()
	const iterations = 12
	for i := range iterations {
		dirName := fmt.Sprintf("sub-%02d", i)
		subAdm, subReq := testAdmitSubdir(t, session, intent, rootAdm, dirName, byte(0x60+i*4))
		payload := fmt.Appendf(nil, "payload-%02d", i)
		file := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, dirName, "item.bin", byte(0x62+i*4), uint64(len(payload)))

		gate := make(chan struct{})
		var (
			beginStart transfer.FileStart
			beginErr   error
			finSettled transfer.DirectorySettlement
			finErr     error
			wg         sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			beginStart, beginErr = session.BeginFile(ctx, file)
		}()
		go func() {
			defer wg.Done()
			<-gate
			finSettled, finErr = session.FinalizeDirectory(ctx, subAdm)
		}()
		close(gate)
		wg.Wait()

		if beginErr != nil {
			t.Fatalf("iter %d BeginFile: %v", i, beginErr)
		}
		tx, _, ok := beginStart.Transaction()
		if !ok {
			t.Fatalf("iter %d BeginFile did not return transaction", i)
		}
		if err := tx.WriteRange(ctx, 0, payload); err != nil {
			t.Fatalf("iter %d WriteRange: %v", i, err)
		}
		if committed, err := tx.Commit(ctx); err != nil || committed.Kind() != transfer.FilePublished {
			t.Fatalf("iter %d Commit = (%v, %v)", i, committed.Kind(), err)
		}

		if finErr != nil {
			if !errors.Is(finErr, transfer.ErrInvalidOutputBinding) {
				t.Fatalf("iter %d concurrent FinalizeDirectory err = %v, want ErrInvalidOutputBinding", i, finErr)
			}
			if settled, err := session.FinalizeDirectory(ctx, subAdm); err != nil || settled.Kind() != transfer.DirectoryFinalized {
				t.Fatalf("iter %d follow-up FinalizeDirectory = (%v, %v)", i, settled.Kind(), err)
			}
		} else if finSettled.Kind() != transfer.DirectoryFinalized {
			t.Fatalf("iter %d concurrent FinalizeDirectory kind = %v, want DirectoryFinalized", i, finSettled.Kind())
		}

		got, err := os.ReadFile(filepath.Join(root, res.PhysicalName(), dirName, "item.bin"))
		if err != nil || string(got) != string(payload) {
			t.Fatalf("iter %d published file = (%q, %v), want %q", i, got, err, payload)
		}
	}

	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if tree, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("FinalizeTree = (%v, %v)", tree.Kind(), err)
	}
}

func TestSessionConcurrentBeginFileLookupAndPauseTreeIsRaceFree(t *testing.T) {
	ctx := context.Background()
	const iterations = 8
	for i := range iterations {
		root := t.TempDir()
		auth, session, intent, rootAdm := openTestResultRootSession(t, root, byte(0x70+i*4))
		subAdm, subReq := testAdmitSubdir(t, session, intent, rootAdm, "race", byte(0x72+i*4))
		file := testSubdirMaterializationFile(t, session, intent, subAdm, subReq, "race", "data.bin", byte(0x74+i*4), 8)
		canonicalPath := file.MaterializationRelativePath().String()

		gate := make(chan struct{})
		var (
			beginStart  transfer.FileStart
			beginErr    error
			lookupFound bool
			lookupErr   error
			pauseSettle transfer.DirectTreeSettlement
			pauseErr    error
			wg          sync.WaitGroup
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-gate
			beginStart, beginErr = session.BeginFile(ctx, file)
		}()
		go func() {
			defer wg.Done()
			<-gate
			_, lookupFound, lookupErr = session.LookupDelivery(ctx, file.Descriptor().FileID(), canonicalPath)
		}()
		go func() {
			defer wg.Done()
			<-gate
			pauseSettle, pauseErr = session.PauseTree(ctx)
		}()
		close(gate)
		wg.Wait()

		if pauseErr != nil || pauseSettle.Kind() != transfer.DirectTreeSettlementPaused {
			_ = auth.Close()
			t.Fatalf("iter %d PauseTree = (%v, %v), want Paused", i, pauseSettle.Kind(), pauseErr)
		}
		if beginErr != nil {
			if !errors.Is(beginErr, transfer.ErrDirectTreeSessionFatal) {
				_ = auth.Close()
				t.Fatalf("iter %d BeginFile err = %v, want ErrDirectTreeSessionFatal", i, beginErr)
			}
		} else {
			tx, _, ok := beginStart.Transaction()
			if !ok {
				_ = auth.Close()
				t.Fatalf("iter %d BeginFile returned non-transaction start", i)
			}
			// PauseTree must have already settled any transaction admitted before close.
			if writeErr := tx.WriteRange(ctx, 0, []byte("1234")); !errors.Is(writeErr, transfer.ErrInvalidOutputBinding) {
				_ = auth.Close()
				t.Fatalf("iter %d post-PauseTree WriteRange = %v, want ErrInvalidOutputBinding", i, writeErr)
			}
		}
		if lookupFound || (lookupErr != nil && !errors.Is(lookupErr, transfer.ErrDirectTreeSessionFatal)) {
			_ = auth.Close()
			t.Fatalf("iter %d LookupDelivery = (found=%t, err=%v), want (false, nil|ErrDirectTreeSessionFatal)", i, lookupFound, lookupErr)
		}
		if err := auth.Close(); err != nil {
			t.Fatalf("iter %d auth.Close: %v", i, err)
		}
	}
}
