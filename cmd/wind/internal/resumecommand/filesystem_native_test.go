//go:build windows || linux

package resumecommand

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const nativeResumeControlDirectoryName = ".windshare-output"

func TestFilesystemResumeInventoryBindsExplicitRootWithoutCreatingGlobalState(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	inventory, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(
		context.Background(), root,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.operations) != 0 || snapshot.registryUnknown {
		t.Fatalf("empty native inventory=(%+v, %v)", snapshot, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only inventory created state: entries=%v err=%v", entries, err)
	}
}

func TestFilesystemResumeInventoryNeverCreatesAMissingRequestedRoot(t *testing.T) {
	parent := newResumeCertifiedOutputTestRoot(t)
	missing := filepath.Join(parent, "missing")
	if _, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(
		context.Background(), missing,
	); err == nil {
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
	inventory, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.operations) != 1 {
		t.Fatalf("operations=(%+v, %v)", snapshot.operations, err)
	}
	wantOperation := hex.EncodeToString(intent.OperationID().Bytes())
	if snapshot.operations[0].operationID != wantOperation ||
		snapshot.operations[0].state != resumeOperationIncomplete ||
		snapshot.operations[0].running || len(snapshot.operations[0].blockedItems) != 0 {
		t.Fatalf("projected operation=%+v", snapshot.operations[0])
	}
	report, err := inventory.Discard(ctx, 0)
	if err != nil || report.status != resumeDiscardStatusDiscarded || report.operationID != wantOperation {
		t.Fatalf("discard report=(%+v, %v)", report, err)
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil || !bytes.Equal(got, foreignContent) {
		t.Fatalf("foreign object changed: content=%q err=%v", got, err)
	}

	reopened, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Snapshot()
	if err != nil || len(after.operations) != 0 {
		t.Fatalf("terminal provenance remained after cleanup: snapshot=%+v err=%v", after, err)
	}
}

func TestFilesystemResumeInventoryReportsLeaseContentionWithoutCheckpointGuessing(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	output, _ := createNativeResumeOperation(t, ctx, root, 2)
	t.Cleanup(func() { _ = output.Close() })

	inventory, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inventory.Snapshot()
	if err != nil || len(snapshot.operations) != 1 || !snapshot.operations[0].running ||
		len(snapshot.operations[0].blockedItems) != 0 {
		t.Fatalf("busy snapshot=(%+v, %v)", snapshot, err)
	}
	if _, err := inventory.Discard(ctx, 0); !errors.Is(err, osfs.ErrResumeStateBusy) {
		t.Fatalf("busy discard error=%v", err)
	}
}

func TestFilesystemResumeRunnerPreservesCorruptUnknownControlOwnership(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	foreignControl := filepath.Join(root, nativeResumeControlDirectoryName)
	content := []byte("not WindShare-owned control state")
	if err := os.WriteFile(foreignControl, content, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runner := NewFilesystemRunner(FilesystemConfig{
		Input: strings.NewReader(""), Output: stdout,
		RawTerminalOutput: stderr, SerializedTerminalOutput: stderr,
		Logf: func(format string, args ...any) {
			_, _ = stderr.WriteString(format)
		},
	})
	if result := runner.Run(context.Background(), []string{"list", "-o", root}); result != ResultFailure {
		t.Fatalf("result=%d", result)
	}
	if !strings.Contains(stdout.String(), `resume_list_status="needs-attention"`) ||
		!strings.Contains(stdout.String(), resumeDestinationBindingReason) ||
		!strings.Contains(stdout.String(), `stage="destination_binding"`) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
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
