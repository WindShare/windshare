//go:build windows || linux

package filesystem

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/engine/recovery"
)

const nativeResumeControlDirectoryName = ".windshare-tmp"

func TestFilesystemResumeInventoryBindsExplicitRootWithoutCreatingGlobalState(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	inventory, err := recovery.Inspect(context.Background(), Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.Operations) != 0 || snapshot.RegistryUnknown {
		t.Fatalf("empty native inventory=(%+v, %v)", snapshot, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only inventory created State: entries=%v err=%v", entries, err)
	}
}

func TestFilesystemResumeInventoryNeverCreatesAMissingRequestedRoot(t *testing.T) {
	parent := newResumeCertifiedOutputTestRoot(t)
	missing := filepath.Join(parent, "missing")
	if _, err := recovery.Inspect(context.Background(), Recovery(missing)); err == nil {
		t.Fatal("missing resume root was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root was mutated: %v", err)
	}
}

func TestFilesystemResumeInventoryRestartsAndDiscardsExactOperationWithoutDeletingForeignObjects(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	authority, intent := createNativeResumeOperation(t, ctx, root, 1)
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		t.Fatal("operation omitted its named destination reservation")
	}
	foreignPath := filepath.Join(root, reservation.PhysicalName(), "foreign.txt")
	foreignContent := []byte("foreign content must survive discard")
	if err := os.WriteFile(foreignPath, foreignContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	// Closing the original authority simulates a process boundary: the command
	// must reacquire root identity, registry ownership, and the exact operation.
	inventory, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 {
		t.Fatalf("Operations=(%+v, %v)", snapshot.Operations, err)
	}
	wantOperation := intent.OperationID()
	if snapshot.Operations[0].ID != wantOperation ||
		snapshot.Operations[0].State != recovery.OperationIncomplete ||
		snapshot.Operations[0].Running || len(snapshot.Operations[0].BlockedItems) != 0 {
		t.Fatalf("projected operation=%+v", snapshot.Operations[0])
	}
	result := inventory.Discard(ctx, snapshot.Operations[0].ID)
	report, err := result.Report, result.Err
	if err != nil || report.Status != recovery.Discarded || report.ID != wantOperation {
		t.Fatalf("discard report=(%+v, %v)", report, err)
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(got, foreignContent) {
		t.Fatalf("foreign object changed: content=%q err=%v", got, err)
	}

	reopened, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Snapshot()
	if err != nil || len(after.Operations) != 0 {
		t.Fatalf("terminal provenance remained after cleanup: snapshot=%+v err=%v", after, err)
	}
}

func TestFilesystemResumeInventoryReportsLeaseContentionWithoutCheckpointGuessing(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	authority, _ := createNativeResumeOperation(t, ctx, root, 2)
	t.Cleanup(func() { _ = authority.Close() })

	inventory, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 || !snapshot.Operations[0].Running ||
		len(snapshot.Operations[0].BlockedItems) != 0 {
		t.Fatalf("busy snapshot=(%+v, %v)", snapshot, err)
	}
	if result := inventory.Discard(ctx, snapshot.Operations[0].ID); !errors.Is(result.Err, recovery.ErrBusy) && result.Failure.Kind != recovery.FailureBusy {
		t.Fatalf("busy discard result=%+v", result)
	}
}

func TestFilesystemResumeRunnerPreservesCorruptUnknownControlOwnership(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	foreignControl := filepath.Join(root, nativeResumeControlDirectoryName)
	content := []byte("not WindShare-owned control State")
	if err := os.WriteFile(foreignControl, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := recovery.Inspect(context.Background(), Recovery(root))
	var failure *recovery.Failure
	if !errors.As(err, &failure) || failure.Detail.Stage != output.FailureDestinationBinding {
		t.Fatalf("unverified control ownership error=%v", err)
	}
	got, err := os.ReadFile(foreignControl)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("unknown control object changed: content=%q err=%v", got, err)
	}
}

func createNativeResumeOperation(
	t *testing.T,
	ctx context.Context,
	root string,
	seed byte,
) (output.Authority, transfer.ReceiveIntent) {
	t.Helper()
	authority, err := FilesystemOutput{RootPath: root, CreateRoot: true}.NewOutputAuthority(output.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var share catalog.ShareInstance
	share[0] = seed
	var syntheticRoot catalog.DirectoryID
	syntheticRoot[0] = seed + 1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatalf("BindDestination() = %v", err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind != output.OutputLookupCreateReady || lookup.Reservation == nil {
		t.Fatalf("active lookup = (%+v, %v)", lookup, err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Reservation.Create(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return authority, operation.Intent
}

func newResumeCertifiedOutputTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	testBase := filepath.Join(home, ".windshare-test-temp")
	if err := os.MkdirAll(testBase, 0o700); err != nil {
		t.Fatal(err)
	}
	reserved, err := os.MkdirTemp(testBase, ".windshare-resume-command-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(reserved); err != nil {
			t.Errorf("remove certified resume command test root: %v", err)
		}
	})
	return reserved
}

func TestFilesystemRecoveryStaleSelectionCannotDiscardReplacementOperation(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	original, intent := createNativeResumeOperation(t, ctx, root, 10)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	inventory, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	if report, err := Recovery(root).Discard(ctx, intent.OperationID()); err != nil || report.Status != recovery.Discarded {
		t.Fatalf("concurrent discard=(%+v, %v)", report, err)
	}
	replacement, replacementIntent := createNativeResumeOperation(t, ctx, root, 20)
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	result := inventory.Discard(ctx, intent.OperationID())
	if result.Successful() || result.Failure == nil || result.Failure.Kind != recovery.FailureChanged {
		t.Fatalf("stale selection=%+v", result)
	}
	current, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := current.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 || snapshot.Operations[0].ID != replacementIntent.OperationID() {
		t.Fatalf("replacement operation changed: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestFilesystemRecoveryDiscardRejectsReplacedOperationIdentityAndPreservesData(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	original, originalIntent := createNativeResumeOperation(t, ctx, root, 30)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}

	// Inspect captures the listed identity before releasing all locks, matching
	// the interactive confirmation gap before Discard reacquires them.
	inventory, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatal(err)
	}

	replacement, replacementIntent := createNativeResumeOperation(t, ctx, root, 40)
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	originalDir := filepath.Join(root, nativeResumeControlDirectoryName, hex.EncodeToString(originalIntent.OperationID().Bytes()))
	replacementDir := filepath.Join(root, nativeResumeControlDirectoryName, hex.EncodeToString(replacementIntent.OperationID().Bytes()))
	if err := os.RemoveAll(originalDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementDir, originalDir); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(originalDir, "retained.part")
	payloadContent := []byte("replacement operation data must survive")
	if err := os.WriteFile(payloadPath, payloadContent, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(originalDir, "task.lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	result := inventory.Discard(ctx, originalIntent.OperationID())
	if result.Successful() || result.Failure == nil ||
		result.Failure.Kind != recovery.FailureChanged ||
		result.Failure.Reason != "operation-no-longer-matches" ||
		!errors.Is(result.Err, output.ErrOperationChanged) {
		t.Fatalf("replaced-identity discard=%+v", result)
	}
	gotPayload, err := os.ReadFile(payloadPath)
	if err != nil || !bytes.Equal(gotPayload, payloadContent) {
		t.Fatalf("replacement payload changed: content=%q err=%v", gotPayload, err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused discard created task.lock in replaced dir: %v", err)
	}
}

func TestFilesystemResumeInventoryListsSiblingWhenRunningOperationHasRevisionConflict(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)

	siblingAuth, siblingIntent := createNativeResumeOperation(t, ctx, root, 50)
	if err := siblingAuth.Close(); err != nil {
		t.Fatal(err)
	}

	conflictedAuth, conflictedSession, conflictedIntent, conflictPath := openRunningConflictedNativeOperation(t, ctx, root, 60)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = conflictedAuth.Close()
		}
	})

	inventory, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatalf("Inspect() with running conflicted operation = %v, want nil", err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || snapshot.RegistryUnknown || len(snapshot.Operations) != 2 {
		t.Fatalf("running snapshot = (%+v, %v), want 2 operations", snapshot, err)
	}

	byID := make(map[receivecontract.OperationID]recovery.Operation, len(snapshot.Operations))
	for _, op := range snapshot.Operations {
		byID[op.ID] = op
	}
	runningOp, ok := byID[conflictedIntent.OperationID()]
	if !ok || !runningOp.Running || runningOp.State != recovery.OperationIncomplete ||
		runningOp.Attention != recovery.AttentionNone || len(runningOp.BlockedItems) != 0 {
		t.Fatalf("running conflicted op = (%+v, found=%t), want running incomplete without blocked items", runningOp, ok)
	}
	siblingOp, ok := byID[siblingIntent.OperationID()]
	if !ok || siblingOp.Running || siblingOp.State != recovery.OperationIncomplete || len(siblingOp.BlockedItems) != 0 {
		t.Fatalf("sibling op = (%+v, found=%t), want idle incomplete", siblingOp, ok)
	}

	if busyResult := inventory.Discard(ctx, runningOp.ID); busyResult.Failure == nil || busyResult.Failure.Kind != recovery.FailureBusy {
		t.Fatalf("discard running conflicted op = %+v, want FailureBusy", busyResult)
	}
	if siblingResult := inventory.Discard(ctx, siblingOp.ID); !siblingResult.Successful() {
		t.Fatalf("discard idle sibling while conflicted op runs = %+v, want success", siblingResult)
	}

	if _, err := conflictedSession.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conflictedAuth.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	afterClose, err := recovery.Inspect(ctx, Recovery(root))
	if err != nil {
		t.Fatalf("Inspect() after closing conflicted operation = %v", err)
	}
	afterSnapshot, err := afterClose.Snapshot()
	if err != nil || len(afterSnapshot.Operations) != 1 {
		t.Fatalf("idle snapshot = (%+v, %v), want 1 operation", afterSnapshot, err)
	}
	idleOp := afterSnapshot.Operations[0]
	wantBlocked := recovery.BlockedItem{
		ArtifactPath: conflictPath,
		PathKnown:    true,
		Reason:       recovery.BlockedRevisionConflict,
	}
	if idleOp.ID != conflictedIntent.OperationID() || idleOp.Running ||
		len(idleOp.BlockedItems) != 1 || idleOp.BlockedItems[0] != wantBlocked {
		t.Fatalf("idle conflicted op = %+v, want blocked item %+v", idleOp, wantBlocked)
	}
}

func openRunningConflictedNativeOperation(
	t *testing.T,
	ctx context.Context,
	root string,
	seed byte,
) (output.Authority, transfer.DirectTreeSession, transfer.ReceiveIntent, string) {
	t.Helper()
	authority, err := FilesystemOutput{RootPath: root, CreateRoot: true}.NewOutputAuthority(output.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var share catalog.ShareInstance
	share[0] = seed
	var syntheticRoot catalog.DirectoryID
	syntheticRoot[0] = seed + 1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	if err := authority.BindDestination(ctx); err != nil {
		_ = authority.Close()
		t.Fatalf("BindDestination() = %v", err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind != output.OutputLookupCreateReady || lookup.Reservation == nil {
		_ = authority.Close()
		t.Fatalf("SelectOperation() = (%+v, %v)", lookup, err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	operation, err := lookup.Reservation.Create(ctx, artifact)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	session, err := operation.Materializer.OpenDirectTree(ctx, operation.Intent)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}

	var dirGen catalog.DirectoryGeneration
	dirGen[0] = seed + 2
	rootReq, err := transfer.NewDirectoryMaterializationRequest(
		operation.Intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: operation.Intent.SyntheticRoot(),
			Generation:  dirGen,
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	rootAdm, err := session.AdmitDirectory(ctx, rootReq)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}

	var fileID catalog.FileID
	fileID[0] = seed + 3
	var rev1, rev2 content.FileRevision
	rev1[0] = seed + 4
	rev2[0] = seed + 5
	initialFile := buildNativeMaterializationFile(t, operation.Intent, session.SessionID(), rootReq, rootAdm, fileID, rev1, "drift.bin", 8)
	start, err := session.BeginFile(ctx, initialFile)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		_ = authority.Close()
		t.Fatal("expected initial file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte("1234")); err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	if paused, err := tx.Pause(ctx); err != nil || paused.Kind() != transfer.FilePaused {
		_ = authority.Close()
		t.Fatalf("Pause() = (%v, %v)", paused.Kind(), err)
	}

	changedFile := buildNativeMaterializationFile(t, operation.Intent, session.SessionID(), rootReq, rootAdm, fileID, rev2, "drift.bin", 12)
	blockedStart, err := session.BeginFile(ctx, changedFile)
	if err != nil {
		_ = authority.Close()
		t.Fatal(err)
	}
	settled, immediate := blockedStart.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FileItemBlocked {
		_ = authority.Close()
		t.Fatalf("changed revision start = (immediate=%t, kind=%v), want FileItemBlocked", immediate, settled.Kind())
	}
	return authority, session, operation.Intent, changedFile.MaterializationRelativePath().String()
}

func buildNativeMaterializationFile(
	t *testing.T,
	intent transfer.ReceiveIntent,
	sessionID transfer.OutputSessionID,
	rootReq transfer.DirectoryMaterializationRequest,
	rootAdm transfer.DirectoryAdmission,
	fileID catalog.FileID,
	rev content.FileRevision,
	name string,
	size uint64,
) transfer.MaterializationFile {
	t.Helper()
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(intent.ShareInstance(), fileID, rev, geom, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := transfer.NewMaterializedDirectoryClaim(rootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	parentSrc := rootReq.Source()
	fileParent, err := transfer.NewDirectoryMaterializationFileParent(
		parentSrc.DirectoryID, parentSrc.Generation, parentSrc.SourcePath, rootAdm, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := transfer.NewMaterializationRootRelativePath(rootAdm.Path() + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(intent, sourcePath, rel, desc, sessionID, fileParent)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
