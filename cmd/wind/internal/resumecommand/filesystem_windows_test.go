//go:build windows

package resumecommand

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/output"
	"golang.org/x/sys/windows"
)

func TestWindowsCLIResumeDiscardRetriesAfterPartHandleReleased(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opIDA := createPausedCLISingleFileOp(t, ctx, root, 0x31, "locked.bin")
	opIDB := createPausedCLISingleFileOp(t, ctx, root, 0x41, "sibling.bin")
	opHexA := hex.EncodeToString(opIDA.Bytes())
	opHexB := hex.EncodeToString(opIDB.Bytes())

	partPath := findCLIPartFile(t, filepath.Join(root, ".windshare-tmp", opHexA))
	lockHandle := openFileWithoutDeleteShare(t, partPath)
	closed := false
	closeLock := func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(lockHandle)
		}
	}
	t.Cleanup(closeLock)

	application, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			t.Error(closeErr)
		}
	})

	runCLI := func(confirmLine string, args ...string) (command.ExitCode, string, string) {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		logger := logFunc(func(format string, values ...any) { _, _ = fmt.Fprintf(stderr, format+"\n", values...) })
		runner := newRunner(resumeDependencies{
			inventories:  filesystemResumeStateInventoryOpener{recovery: application},
			confirmation: &fakeResumeConfirmationTerminal{interactive: true, line: confirmLine},
			parser:       flagRequestParser{logger: logger},
			renderer:     textRenderer{},
			output:       streamResumeOutput{result: stdout, usage: testDiagnosticOutput{writer: stderr}},
			logger:       logger,
		})
		code := runner.Run(ctx, args)
		return code, stdout.String(), stderr.String()
	}

	code, listOut, listErr := runCLI("", "list", "-o", root)
	if code != command.ExitOK || !strings.Contains(listOut, `resume_list_status="ready" operations=2 registry_unknown=false`) {
		t.Fatalf("initial list = (code=%d stdout=%q stderr=%q)", code, listOut, listErr)
	}
	itemA := itemNumberForOperation(t, listOut, opHexA)

	// 1. Discard while .part is held without FILE_SHARE_DELETE -> cleanup-pending.
	code, discardOut, discardErr := runCLI(fmt.Sprintf("discard %d", itemA), "discard", "-o", root, "--item", fmt.Sprintf("%d", itemA))
	if code != command.ExitFailure ||
		!strings.Contains(discardOut, `resume_discard_status="cleanup-pending"`) ||
		!strings.Contains(discardOut, `reason="cleanup-uncertain"`) {
		t.Fatalf("locked discard = (code=%d stdout=%q stderr=%q)", code, discardOut, discardErr)
	}

	// 2. List must keep opA visible as cleanup-pending and NOT mark registry_unknown=true.
	code, listOut, listErr = runCLI("", "list", "-o", root)
	if code != command.ExitOK ||
		!strings.Contains(listOut, `resume_list_status="ready" operations=2 registry_unknown=false`) ||
		!strings.Contains(listOut, fmt.Sprintf(`state="cleanup-pending" operation_id=%q`, opHexA)) {
		t.Fatalf("list after locked discard = (code=%d stdout=%q stderr=%q)", code, listOut, listErr)
	}

	// 3. Sibling discard in the same destination must stay unblocked.
	itemB := itemNumberForOperation(t, listOut, opHexB)
	code, discardOut, discardErr = runCLI(fmt.Sprintf("discard %d", itemB), "discard", "-o", root, "--item", fmt.Sprintf("%d", itemB))
	if code != command.ExitOK || !strings.Contains(discardOut, `resume_discard_status="discarded"`) {
		t.Fatalf("sibling discard = (code=%d stdout=%q stderr=%q)", code, discardOut, discardErr)
	}

	// 4. Release handle and retry discard through the ordinary CLI.
	closeLock()
	code, listOut, listErr = runCLI("", "list", "-o", root)
	if code != command.ExitOK ||
		!strings.Contains(listOut, `resume_list_status="ready" operations=1 registry_unknown=false`) ||
		!strings.Contains(listOut, fmt.Sprintf(`resume_operation=1 state="cleanup-pending" operation_id=%q`, opHexA)) {
		t.Fatalf("list after release = (code=%d stdout=%q stderr=%q)", code, listOut, listErr)
	}
	code, discardOut, discardErr = runCLI("discard 1", "discard", "-o", root, "--item", "1")
	if code != command.ExitOK || !strings.Contains(discardOut, `resume_discard_status="discarded"`) {
		t.Fatalf("retry discard = (code=%d stdout=%q stderr=%q)", code, discardOut, discardErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".windshare-tmp")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf(".windshare-tmp survived retry discard: %v", statErr)
	}
	code, listOut, listErr = runCLI("", "list", "-o", root)
	if code != command.ExitOK || !strings.Contains(listOut, `resume_list_status="ready" operations=0 registry_unknown=false`) {
		t.Fatalf("final list = (code=%d stdout=%q stderr=%q)", code, listOut, listErr)
	}
}

func createPausedCLISingleFileOp(
	t *testing.T,
	ctx context.Context,
	root string,
	seed byte,
	name string,
) receivecontract.OperationID {
	t.Helper()
	authority, err := filesystem.FilesystemOutput{RootPath: root, CreateRoot: true}.NewOutputAuthority(output.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if err := authority.BindDestination(ctx); err != nil {
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
	sel, err := transfer.NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind != output.OutputLookupCreateReady {
		t.Fatalf("SelectOperation = (%+v, %v)", lookup, err)
	}
	var fileID catalog.FileID
	fileID[0] = seed + 2
	artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, name, name)
	if err != nil {
		t.Fatal(err)
	}
	op, err := lookup.Reservation.Create(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	session, err := op.Materializer.OpenDirectTree(ctx, op.Intent)
	if err != nil {
		t.Fatal(err)
	}
	geom, err := content.NewFileGeometry(8, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var rev content.FileRevision
	rev[0] = seed + 3
	desc, err := content.NewFileRevisionDescriptor(op.Intent.ShareInstance(), fileID, rev, geom, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	var dirGen catalog.DirectoryGeneration
	dirGen[0] = seed + 4
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := transfer.NewReferenceMaterializationFileParent(op.Intent.SyntheticRoot(), dirGen, ordinaryoutput.EmptySourceCatalogPath())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := transfer.NewMaterializationRootRelativePath(name)
	if err != nil {
		t.Fatal(err)
	}
	matFile, err := transfer.NewMaterializationFile(op.Intent, sourcePath, rel, desc, session.SessionID(), parent)
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(ctx, matFile)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	return op.Intent.OperationID()
}

func findCLIPartFile(t *testing.T, opDir string) string {
	t.Helper()
	entries, err := os.ReadDir(opDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			return filepath.Join(opDir, entry.Name())
		}
	}
	t.Fatalf("no .part file in %q", opDir)
	return ""
}

func openFileWithoutDeleteShare(t *testing.T, path string) windows.Handle {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q): %v", path, err)
	}
	return h
}

func itemNumberForOperation(t *testing.T, listOutput, opHex string) int {
	t.Helper()
	needle := fmt.Sprintf("operation_id=%q", opHex)
	for line := range strings.SplitSeq(listOutput, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		var item int
		if _, err := fmt.Sscanf(line, "resume_operation=%d ", &item); err == nil && item > 0 {
			return item
		}
	}
	t.Fatalf("operation %s not found in list output:\n%s", opHex, listOutput)
	return 0
}
