//go:build windows || linux

package recovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const nativeResumeControlDirectoryName = ".windshare-output"

func TestFilesystemResumeInventoryBindsExplicitRootWithoutCreatingGlobalState(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	inventory, err := Inspect(context.Background(), Filesystem(root))
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
	if _, err := Inspect(context.Background(), Filesystem(missing)); err == nil {
		t.Fatal("missing resume root was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root was mutated: %v", err)
	}
}

func TestFilesystemResumeInventoryRestartsAndDiscardsExactOperationWithoutDeletingForeignObjects(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	output, intent := createNativeResumeOperation(t, ctx, root, 1)
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		t.Fatal("operation omitted its named destination reservation")
	}
	foreignPath := filepath.Join(root, reservation.PhysicalName(), "foreign.txt")
	foreignContent := []byte("foreign content must survive discard")
	if err := os.WriteFile(foreignPath, foreignContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	// Closing the original authority simulates a process boundary: the command
	// must reacquire root identity, registry ownership, and the exact operation.
	inventory, err := Inspect(ctx, Filesystem(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 {
		t.Fatalf("Operations=(%+v, %v)", snapshot.Operations, err)
	}
	wantOperation := intent.OperationID()
	if snapshot.Operations[0].ID != wantOperation ||
		snapshot.Operations[0].State != OperationIncomplete ||
		snapshot.Operations[0].Running || len(snapshot.Operations[0].BlockedItems) != 0 {
		t.Fatalf("projected operation=%+v", snapshot.Operations[0])
	}
	result := inventory.Discard(ctx, snapshot.Operations[0].ID)
	report, err := result.Report, result.Err
	if err != nil || report.Status != Discarded || report.ID != wantOperation {
		t.Fatalf("discard report=(%+v, %v)", report, err)
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(got, foreignContent) {
		t.Fatalf("foreign object changed: content=%q err=%v", got, err)
	}

	reopened, err := Inspect(ctx, Filesystem(root))
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
	output, _ := createNativeResumeOperation(t, ctx, root, 2)
	t.Cleanup(func() { _ = output.Close() })

	inventory, err := Inspect(ctx, Filesystem(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 || !snapshot.Operations[0].Running ||
		len(snapshot.Operations[0].BlockedItems) != 0 {
		t.Fatalf("busy snapshot=(%+v, %v)", snapshot, err)
	}
	if result := inventory.Discard(ctx, snapshot.Operations[0].ID); !errors.Is(result.Err, osfs.ErrResumeStateBusy) && result.Failure.Kind != FailureBusy {
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
	_, err := Inspect(context.Background(), Filesystem(root))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Detail.Stage != osfs.FilesystemOutputFailureDestinationBinding {
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
) (*osfs.FilesystemOutputAuthority, transfer.ReceiveIntent) {
	t.Helper()
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
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
	mode, err := output.BindDestination(ctx)
	if err != nil || !mode.Resumable() {
		t.Fatalf("destination mode = (%+v, %v)", mode, err)
	}
	lookup, err := output.LookupActive(ctx, selection)
	if err != nil || lookup.Kind() != osfs.FilesystemOutputLookupMiss {
		t.Fatalf("active lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := output.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("created operation omitted its frozen receive intent")
	}
	return output, intent
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
	inventory, err := Inspect(ctx, Filesystem(root))
	if err != nil {
		t.Fatal(err)
	}
	if report, err := Filesystem(root).Discard(ctx, intent.OperationID()); err != nil || report.Status != Discarded {
		t.Fatalf("concurrent discard=(%+v, %v)", report, err)
	}
	replacement, replacementIntent := createNativeResumeOperation(t, ctx, root, 20)
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	result := inventory.Discard(ctx, intent.OperationID())
	if result.Successful() || result.Failure == nil || result.Failure.Kind != FailureChanged {
		t.Fatalf("stale selection=%+v", result)
	}
	current, err := Inspect(ctx, Filesystem(root))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := current.Snapshot()
	if err != nil || len(snapshot.Operations) != 1 || snapshot.Operations[0].ID != replacementIntent.OperationID() {
		t.Fatalf("replacement operation changed: snapshot=%+v err=%v", snapshot, err)
	}
}
