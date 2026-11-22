//go:build windows || linux || darwin

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/output"
)

func TestRecoveryAuthorityListsAbsentOrdinaryStateWithoutMutation(t *testing.T) {
	root := t.TempDir()
	authority := NewRecoveryAuthority(root)
	inventory, err := authority.List(context.Background())
	if err != nil || inventory.Status() != RecoveryListReady ||
		len(inventory.Summaries()) != 0 || inventory.UnknownEntries() {
		t.Fatalf("absent inventory = (%+v, %v)", inventory, err)
	}
	_, err = os.Lstat(filepath.Join(root, output.ControlDirName))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resume list created control state: %v", err)
	}
	if _, err := NewRecoveryAuthority("relative").List(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative resume root error = %v", err)
	}
}

func openPublicOrdinaryOperation(
	t *testing.T,
	root string,
	seed byte,
) (*OutputAuthority, *OutputOperation, transfer.ReceiveIntent) {
	t.Helper()
	authority, err := NewOutputAuthority(OutputConfig{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
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
	lookup, err := authority.SelectOperation(context.Background(), selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupCreateReady {
		t.Fatalf("active lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok || intent.IsZero() {
		t.Fatal("created operation omitted its frozen receive intent")
	}
	return authority, operation, intent
}

func TestOutputAuthorityLifecycleAndNilGuards(t *testing.T) {
	var nilAuthority *OutputAuthority
	if err := nilAuthority.BindDestination(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil bind error = %v", err)
	}
	if _, err := nilAuthority.SelectOperation(
		context.Background(), transfer.SelectionSpec{}, receivecontract.AutomaticOperation(),
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil select error = %v", err)
	}
	if err := nilAuthority.Close(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "facade-values")
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0xe1)
	received, ok := operation.ReceiveIntent()
	if !ok || !received.Equal(intent) {
		t.Fatal("operation changed the frozen intent")
	}
	session, err := operation.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.PauseTree(context.Background()); err != nil || settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryAuthorityListsAndDiscardsActiveOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned-output")
	foreignPath := filepath.Join(root, "caller-owned.txt")
	authority, _, intent := openPublicOrdinaryOperation(t, root, 0xc1)
	if err := os.WriteFile(foreignPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	recoveryAuthority := NewRecoveryAuthority(root)
	inventory, err := recoveryAuthority.List(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != RecoveryListReady ||
		len(summaries) != 1 ||
		summaries[0].OperationID() != intent.OperationID() ||
		summaries[0].ReceiveIntentDigest() != intent.Digest() ||
		summaries[0].State() != RecoveryOperationIncomplete {
		t.Fatalf("owned inventory = (%+v, %v)", inventory, err)
	}

	discarded, err := recoveryAuthority.Discard(context.Background(), intent.OperationID())
	if err != nil || discarded.State() != RecoveryOperationDiscarded ||
		discarded.OperationID() != intent.OperationID() {
		t.Fatalf("zero-file discard = (%+v, %v)", discarded, err)
	}
	after, err := recoveryAuthority.List(context.Background())
	if err != nil || len(after.Summaries()) != 0 {
		t.Fatalf("terminal row survived discard = (%+v, %v)", after, err)
	}
	if content, err := os.ReadFile(foreignPath); err != nil || string(content) != "preserve" {
		t.Fatalf("discard mutated foreign public output = (%q, %v)", content, err)
	}
}

func TestRecoveryAuthorityExposesStableBusyStateAndError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "busy-output")
	authority, _, intent := openPublicOrdinaryOperation(t, root, 0xd1)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = authority.Close()
		}
	})
	recoveryAuthority := NewRecoveryAuthority(root)
	inventory, err := recoveryAuthority.List(context.Background())
	summaries := inventory.Summaries()
	if err != nil || len(summaries) != 1 || !summaries[0].Busy() ||
		summaries[0].State() != RecoveryOperationIncomplete {
		t.Fatalf("busy inventory = (%+v, %v)", inventory, err)
	}
	if _, err := recoveryAuthority.Discard(
		context.Background(), intent.OperationID(),
	); !errors.Is(err, ErrOutputBusy) {
		t.Fatalf("busy discard error = %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reacquired, err := recoveryAuthority.List(context.Background())
	if err != nil || len(reacquired.Summaries()) != 1 ||
		reacquired.Summaries()[0].Busy() {
		t.Fatalf("released inventory = (%+v, %v)", reacquired, err)
	}
}

func TestRecoveryAuthorityDoesNotRetainCompletedEmptyDirectoryHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty-directory")
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0xe1)
	session, err := operation.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	rootRequest, err := transfer.NewDirectoryMaterializationRequest(
		intent,
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
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	recoveryAuthority := NewRecoveryAuthority(root)
	inventory, err := recoveryAuthority.List(context.Background())
	if err != nil || len(inventory.Summaries()) != 0 {
		t.Fatalf("completed operation retained terminal history = (%+v, %v)", inventory, err)
	}
}
