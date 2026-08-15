//go:build windows || linux

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestFilesystemResumeStateAuthorityListsAbsentOrdinaryStateWithoutMutation(t *testing.T) {
	root := t.TempDir()
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListReady ||
		len(inventory.Summaries()) != 0 || inventory.UnknownEntries() {
		t.Fatalf("absent inventory = (%+v, %v)", inventory, err)
	}
	_, err = os.Lstat(filepath.Join(root, checkpointstore.ControlDirectory))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resume list created control state: %v", err)
	}
	if _, err := NewFilesystemResumeStateAuthority(
		FilesystemResumeRoot{RootPath: "relative"},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative resume root error = %v", err)
	}
}

func openPublicOrdinaryOperation(
	t *testing.T,
	root string,
	seed byte,
) (*FilesystemOutputAuthority, FilesystemOutputOperation, transfer.ReceiveIntent) {
	t.Helper()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mode, err := authority.BindDestination(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !mode.Resumable() {
		_ = authority.Close()
		t.Skip("native test destination does not certify process-restart recovery")
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](seed),
		coverageC6Identity[catalog.DirectoryID](seed+1),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil || lookup.Kind() != FilesystemOutputLookupMiss {
		t.Fatalf("active lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok || intent.IsZero() {
		t.Fatal("created operation omitted its frozen receive intent")
	}
	return authority, operation, intent
}

func TestFilesystemOutputFacadeValueAndNilAuthorityGuards(t *testing.T) {
	var nilAuthority *FilesystemOutputAuthority
	if _, err := nilAuthority.BindDestination(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil bind error = %v", err)
	}
	if _, err := nilAuthority.LookupActive(context.Background(), transfer.SelectionSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil lookup error = %v", err)
	}
	if _, err := nilAuthority.CreateOperation(
		context.Background(), FilesystemOutputLookup{}, receivecontract.ArtifactSpec{},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil create error = %v", err)
	}
	if _, err := nilAuthority.OpenOperation(context.Background(), FilesystemOutputOperation{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil open error = %v", err)
	}
	if _, err := nilAuthority.ReserveDirectTree(
		context.Background(), transfer.SelectionSpec{}, receivecontract.ArtifactSpec{},
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reserve error = %v", err)
	}
	if err := nilAuthority.Close(); err != nil {
		t.Fatal(err)
	}
	if (FilesystemOutputExecutionMode{}).Valid() ||
		(FilesystemOutputLookup{}).Kind() != 0 ||
		(NativeDirectTreeReservation{}).Kind() != 0 {
		t.Fatal("zero facade values became valid")
	}
	if _, ok := (FilesystemOutputLookup{}).Operation().ReceiveIntent(); ok {
		t.Fatal("zero lookup exposed an operation intent")
	}
	if (FilesystemOutputLookup{}).Operation().ExecutionMode().Valid() {
		t.Fatal("zero operation exposed an execution mode")
	}
	if _, ok := (NativeDirectTreeReservation{}).ReceiveIntent(); ok {
		t.Fatal("zero direct reservation exposed an intent")
	}

	root := filepath.Join(t.TempDir(), "facade-values")
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0xe1)
	if mode := operation.ExecutionMode(); !mode.Valid() || !mode.Resumable() || mode.LiveOnly() {
		t.Fatalf("operation mode = %+v", mode)
	}
	received, ok := operation.ReceiveIntent()
	if !ok || !received.EqualCanonical(intent) {
		t.Fatal("operation facade changed the frozen intent")
	}
	session, err := authority.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("facade pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemResumeStateAuthorityListsAndDiscardsActiveOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned-output")
	foreignPath := filepath.Join(root, "caller-owned.txt")
	output, _, intent := openPublicOrdinaryOperation(t, root, 0xc1)
	if err := os.WriteFile(foreignPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != ResumeStateListReady ||
		len(summaries) != 1 ||
		summaries[0].OperationID() != intent.OperationID() ||
		summaries[0].ReceiveIntentDigest() != intent.Digest() ||
		summaries[0].State() != ResumeOperationIncomplete {
		t.Fatalf("owned inventory = (%+v, %v)", inventory, err)
	}

	discarded, err := authority.Discard(context.Background(), intent.OperationID())
	if err != nil || discarded.State() != ResumeOperationDiscarded ||
		discarded.OperationID() != intent.OperationID() {
		t.Fatalf("zero-file discard = (%+v, %v)", discarded, err)
	}
	after, err := authority.ListResumeState(context.Background())
	if err != nil || len(after.Summaries()) != 0 {
		t.Fatalf("terminal row survived discard = (%+v, %v)", after, err)
	}
	if content, err := os.ReadFile(foreignPath); err != nil || string(content) != "preserve" {
		t.Fatalf("discard mutated foreign public output = (%q, %v)", content, err)
	}
}

func TestFilesystemResumeStateAuthorityExposesStableBusyStateAndError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "busy-output")
	output, _, intent := openPublicOrdinaryOperation(t, root, 0xd1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = output.Close()
		}
	})
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || len(summaries) != 1 || !summaries[0].Busy() ||
		summaries[0].State() != ResumeOperationIncomplete {
		t.Fatalf("busy inventory = (%+v, %v)", inventory, err)
	}
	if _, err := authority.Discard(
		context.Background(), intent.OperationID(),
	); !errors.Is(err, ErrResumeStateBusy) {
		t.Fatalf("busy discard error = %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reacquired, err := authority.ListResumeState(context.Background())
	if err != nil || len(reacquired.Summaries()) != 1 ||
		reacquired.Summaries()[0].Busy() {
		t.Fatalf("released inventory = (%+v, %v)", reacquired, err)
	}
}

func TestFilesystemResumeStateAuthorityDoesNotRetainCompletedEmptyDirectoryHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty-directory")
	output, operation, intent := openPublicOrdinaryOperation(t, root, 0xe1)
	session, err := output.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := transfer.OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	rootRequest, err := transfer.NewDirectoryMaterializationRequest(
		projector,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  coverageC6Identity[catalog.DirectoryGeneration](0xe3),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), rootRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	settlement, err := session.FinalizeTree(
		context.Background(), transfer.DirectTreeOutcomeSuccess,
	)
	if err != nil || settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("empty tree settlement = (%d, %v)", settlement.Kind(), err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || len(inventory.Summaries()) != 0 {
		t.Fatalf("completed operation retained terminal history = (%+v, %v)", inventory, err)
	}
}
