package resumecommand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestResumeParsersRequireOneExplicitOutputRootAndOrdinal(t *testing.T) {
	root := t.TempDir()
	app, _, _ := newResumeTestApp()

	request, valid := app.parser().ParseRoot("resume list", []string{"-o", root})
	if !valid || request.rootPath != root {
		t.Fatalf("list request=%+v valid=%t", request, valid)
	}
	discard, valid := app.parser().ParseDiscard([]string{"--item", "2", "-o", root})
	if !valid || discard.rootPath != root || discard.itemNumber != 2 {
		t.Fatalf("discard request=%+v valid=%t", discard, valid)
	}

	for name, args := range map[string][]string{
		"missing root":       {"--item", "1"},
		"missing item":       {"-o", root},
		"repeated root":      {"-o", root, "-o", root, "--item", "1"},
		"zero item":          {"-o", root, "--item", "0"},
		"noncanonical item":  {"-o", root, "--item", "01"},
		"bulk item syntax":   {"-o", root, "--item", "1,2"},
		"repeated item":      {"-o", root, "--item", "1", "--item", "2"},
		"internal path flag": {"-o", root, "--item", "1", "--path", "records/one"},
		"positional path":    {"-o", root, "--item", "1", "records/one"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, valid := app.parser().ParseDiscard(args); valid {
				t.Fatal("invalid discard request was accepted")
			}
		})
	}
	for name, args := range map[string][]string{
		"missing root":  nil,
		"item handle":   {"-o", root, "--item", "1"},
		"repeated root": {"-o", root, "-o", root},
		"positional":    {"-o", root, "records/one"},
	} {
		t.Run("list "+name, func(t *testing.T) {
			if _, valid := app.parser().ParseRoot("resume list", args); valid {
				t.Fatal("invalid list request was accepted")
			}
		})
	}
}

func TestResumeArgumentFailuresNeverReflectRejectedValues(t *testing.T) {
	const accidentalCapability = "windshare://relay.example/#private-capability"
	app, _, stderr := newResumeTestApp()
	if result := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", accidentalCapability,
	}); result != ResultUsage {
		t.Fatalf("result=%d", result)
	}
	if strings.Contains(stderr.String(), accidentalCapability) ||
		!strings.Contains(stderr.String(), "arguments are invalid") {
		t.Fatalf("stderr=%q", stderr.String())
	}

	app, _, stderr = newResumeTestApp()
	if result := app.Run(context.Background(), []string{"resume", accidentalCapability}); result != ResultUsage {
		t.Fatalf("result=%d", result)
	}
	if strings.Contains(stderr.String(), accidentalCapability) ||
		!strings.Contains(stderr.String(), "unknown action") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestResumeListGroupsOperationsAndOnlyShowsBlockedChildren(t *testing.T) {
	root := t.TempDir()
	operations := []resumeOperation{
		testResumeOperation("1", resumeOperationIncomplete),
		testResumeOperation("2", resumeOperationResumable),
		{
			operationID: strings.Repeat("3", 32), state: resumeOperationCleanupPending,
			attention: "cleanup-uncertain",
		},
		{
			operationID: strings.Repeat("4", 32), state: resumeOperationNeedsAttention,
			attention: "operation-ownership-unknown",
			blockedItems: []resumeBlockedItem{
				{artifactPath: "tree/unknown.bin", pathKnown: true, reason: resumeBlockedPublicationUnknown},
				{reason: resumeBlockedCheckpointInvalid},
			},
		},
	}
	snapshot, err := newResumeInventorySnapshot(operations, false)
	if err != nil {
		t.Fatal(err)
	}
	inventory := &fakeResumeStateInventory{snapshot: snapshot}
	opener := &fakeResumeStateInventoryOpener{inventory: inventory}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = opener

	if result := app.Run(context.Background(), []string{"resume", "list", "-o", root}); result != ResultFailure {
		t.Fatalf("result=%d", result)
	}
	if opener.calls != 1 || opener.rootPath != root {
		t.Fatalf("opener=%+v", opener)
	}
	for _, want := range []string{
		`resume_list_status="needs-attention" operations=4 registry_unknown=false`,
		`resume_operation=1 state="incomplete"`,
		`resume_operation=2 state="resumable"`,
		`resume_operation=3 state="cleanup-pending"`,
		`resume_operation=4 state="operation-needs-attention"`,
		`item-blocked path="tree/unknown.bin" reason="publication-unknown"`,
		`item-blocked path_known=false reason="checkpoint-invalid"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
		}
	}
	for _, forbidden := range []string{
		"intent_digest", "state_generation", "expires", "success_count", "failure_count",
		"diagnostic_reference", "partial", "published", "failed",
	} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("stdout exposed %q: %q", forbidden, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "no objects were changed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestResumeListKeepsBusyAsAvailabilityNotLifecycle(t *testing.T) {
	operation := testResumeOperation("1", resumeOperationIncomplete)
	operation.running = true
	snapshot, _ := newResumeInventorySnapshot([]resumeOperation{operation}, false)
	app, stdout, _ := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{
		inventory: &fakeResumeStateInventory{snapshot: snapshot},
	}

	if result := app.Run(context.Background(), []string{
		"resume", "list", "-o", t.TempDir(),
	}); result != ResultOK {
		t.Fatalf("result=%d", result)
	}
	if !strings.Contains(stdout.String(), `state="incomplete"`) ||
		!strings.Contains(stdout.String(), `running=true`) ||
		strings.Contains(stdout.String(), `state="operation-needs-attention"`) {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestResumeListSurfacesUnknownRegistryPagesWithoutDiscardAuthority(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, true,
	)
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{
		inventory: &fakeResumeStateInventory{snapshot: snapshot},
	}
	if result := app.Run(context.Background(), []string{
		"resume", "list", "-o", t.TempDir(),
	}); result != ResultFailure {
		t.Fatalf("result=%d", result)
	}
	if !strings.Contains(stdout.String(), `resume_list_status="needs-attention"`) ||
		!strings.Contains(stdout.String(), `registry_unknown=true`) ||
		!strings.Contains(stdout.String(), `resume_operation=1 state="incomplete"`) ||
		!strings.Contains(stderr.String(), "no objects were changed") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeListSurfacesUnknownOrBusyRootWithoutNativeDetails(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus string
		wantReason string
	}{
		"unknown": {
			err:        errors.New("C:\\private\\control\\record: corrupt checksum"),
			wantStatus: resumeListStatusNeedsAttention,
			wantReason: resumeDestinationUnknownReason,
		},
		"busy": {
			err:        fmt.Errorf("native lock: %w", osfs.ErrResumeStateBusy),
			wantStatus: resumeBusyStatus,
			wantReason: "destination-already-in-use",
		},
		"cancelled": {
			err:        context.Canceled,
			wantStatus: resumeCancelledStatus,
			wantReason: resumeCommandCancelledReason,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			app, stdout, stderr := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{err: test.err}
			if result := app.Run(context.Background(), []string{
				"resume", "list", "-o", t.TempDir(),
			}); result != ResultFailure {
				t.Fatalf("result=%d", result)
			}
			if !strings.Contains(stdout.String(), fmt.Sprintf(`resume_list_status=%q`, test.wantStatus)) ||
				!strings.Contains(stdout.String(), fmt.Sprintf(`reason=%q`, test.wantReason)) {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "private") ||
				strings.Contains(stdout.String()+stderr.String(), "checksum") {
				t.Fatalf("native detail leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestResumeDiscardRequiresFreshOrdinalAndExactTerminalIntent(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationResumable)}, false,
	)
	inventory := &fakeResumeStateInventory{
		snapshot: snapshot,
		discardReport: resumeDiscardReport{
			status: resumeDiscardStatusDiscarded, operationID: strings.Repeat("1", 32),
		},
	}
	terminal := &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = terminal

	if result := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", "1",
	}); result != ResultOK {
		t.Fatalf("result=%d stderr=%q", result, stderr.String())
	}
	if inventory.discardCalls != 1 || inventory.discardIndex != 0 || terminal.calls != 1 {
		t.Fatalf("inventory=%+v terminal=%+v", inventory, terminal)
	}
	for _, want := range []string{
		`resume_operation=1 state="resumable"`,
		`Type "discard 1" exactly`,
		"identity-matched unfinished partial and control records",
		"Final and foreign objects are preserved",
	} {
		if !strings.Contains(terminal.prompt, want) {
			t.Fatalf("prompt=%q missing=%q", terminal.prompt, want)
		}
	}
	for _, want := range []string{
		`resume_discard_status="discarded"`,
		`published_files="preserved"`,
		`foreign_objects="preserved"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
		}
	}
}

func TestResumeDiscardRejectsRedirectedInexactUnknownAndRunningSelections(t *testing.T) {
	base, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	tests := map[string]struct {
		snapshot resumeInventorySnapshot
		terminal *fakeResumeConfirmationTerminal
		status   string
	}{
		"redirected": {
			snapshot: base,
			terminal: &fakeResumeConfirmationTerminal{interactive: false, line: "discard 1"},
			status:   resumeConfirmationStatus,
		},
		"trailing space": {
			snapshot: base,
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1 "},
			status:   resumeNotConfirmedStatus,
		},
		"unknown registry": {
			snapshot: resumeInventorySnapshot{operations: base.operations, registryUnknown: true},
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"},
			status:   resumeDiscardStatusNeedsAttention,
		},
		"running": {
			snapshot: func() resumeInventorySnapshot {
				operation := testResumeOperation("1", resumeOperationIncomplete)
				operation.running = true
				result, _ := newResumeInventorySnapshot([]resumeOperation{operation}, false)
				return result
			}(),
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"},
			status:   resumeBusyStatus,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			inventory := &fakeResumeStateInventory{snapshot: test.snapshot}
			app, stdout, _ := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
			app.resumeConfirmation = test.terminal
			if result := app.Run(context.Background(), []string{
				"resume", "discard", "-o", t.TempDir(), "--item", "1",
			}); result != ResultFailure {
				t.Fatalf("result=%d", result)
			}
			if inventory.discardCalls != 0 ||
				(name == "unknown registry" || name == "running" || name == "redirected") && test.terminal.calls != 0 {
				t.Fatalf("inventory=%+v terminal=%+v", inventory, test.terminal)
			}
			if !strings.Contains(stdout.String(), fmt.Sprintf(`resume_discard_status=%q`, test.status)) {
				t.Fatalf("stdout=%q", stdout.String())
			}
		})
	}
}

func TestResumeDiscardReportsCleanupDebtEvenWhenCleanupReturnsAnError(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	inventory := &fakeResumeStateInventory{
		snapshot: snapshot,
		discardReport: resumeDiscardReport{
			status: resumeDiscardStatusCleanupPending, operationID: strings.Repeat("1", 32),
			attention: "cleanup-uncertain",
		},
		discardErr: errors.New("private control path should not be printed"),
	}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}

	if result := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", "1",
	}); result != ResultFailure {
		t.Fatalf("result=%d", result)
	}
	if !strings.Contains(stdout.String(), `resume_discard_status="cleanup-pending"`) ||
		!strings.Contains(stdout.String(), `reason="cleanup-uncertain"`) ||
		!strings.Contains(stderr.String(), "owned cleanup is incomplete") ||
		strings.Contains(stdout.String()+stderr.String(), "private control path") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeDiscardKeepsACompletedOutcomeWhenAuthorityCloseFails(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	inventory := &fakeResumeStateInventory{
		snapshot: snapshot,
		discardReport: resumeDiscardReport{
			status: resumeDiscardStatusDiscarded, operationID: strings.Repeat("1", 32),
		},
		discardErr: errors.New("close failed"),
	}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
	if result := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", "1",
	}); result != ResultFailure {
		t.Fatalf("result=%d", result)
	}
	if !strings.Contains(stdout.String(), `resume_discard_status="discarded"`) ||
		!strings.Contains(stderr.String(), "was discarded") ||
		strings.Contains(stdout.String()+stderr.String(), "close failed") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeDiscardHandlesLeaseRaceDisappearanceAndUnverifiedFailure(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	tests := map[string]struct {
		err    error
		status string
	}{
		"busy":        {err: osfs.ErrResumeStateBusy, status: resumeBusyStatus},
		"disappeared": {err: fs.ErrNotExist, status: resumeDiscardStatusChanged},
		"cancelled":   {err: context.Canceled, status: resumeCancelledStatus},
		"unknown":     {err: errors.New("ownership changed"), status: resumeDiscardStatusNeedsAttention},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			inventory := &fakeResumeStateInventory{snapshot: snapshot, discardErr: test.err}
			app, stdout, _ := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
			app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
			if result := app.Run(context.Background(), []string{
				"resume", "discard", "-o", t.TempDir(), "--item", "1",
			}); result != ResultFailure {
				t.Fatalf("result=%d", result)
			}
			if !strings.Contains(stdout.String(), fmt.Sprintf(`resume_discard_status=%q`, test.status)) ||
				!strings.Contains(stdout.String(), `foreign_objects="preserved"`) {
				t.Fatalf("stdout=%q", stdout.String())
			}
		})
	}
}

func TestResumeSurfaceHasNoLegacyGlobalOrTerminalHistoryCommands(t *testing.T) {
	for _, action := range []string{"cleanup", "status", "delete", "purge", "cancel", "pause", "complete"} {
		app, _, stderr := newResumeTestApp()
		if result := app.Run(context.Background(), []string{"resume", action}); result != ResultUsage {
			t.Fatalf("action=%q result=%d", action, result)
		}
		if !strings.Contains(stderr.String(), "unknown action") {
			t.Fatalf("action=%q stderr=%q", action, stderr.String())
		}
	}
	app, _, stderr := newResumeTestApp()
	if result := app.Run(context.Background(), []string{"resume", "help"}); result != ResultOK {
		t.Fatalf("result=%d", result)
	}
	help := stderr.String()
	for _, want := range []string{"resume list", "resume discard", "identity-matched", "final and foreign objects stay"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help=%q missing=%q", help, want)
		}
	}
	for _, forbidden := range []string{"resume cleanup", "legacy", "--path", "--all", "terminal state"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help=%q exposed=%q", help, forbidden)
		}
	}
}

func TestStdioResumeConfirmationAndOutputBoundaries(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	if terminal := newStdioResumeConfirmationTerminal(reader, writer, writer); terminal.Interactive() {
		t.Fatal("pipe-backed confirmation was treated as a terminal")
	}

	output := &bytes.Buffer{}
	terminal := stdioResumeConfirmationTerminal{
		input: strings.NewReader("discard 3\r\nignored\n"), output: output, interactive: true,
	}
	line, err := terminal.ReadLine(context.Background(), "confirm: ")
	if err != nil || line != "discard 3" || output.String() != "confirm: " {
		t.Fatalf("line=%q output=%q err=%v", line, output.String(), err)
	}
	if err := (streamResumeOutput{result: shortResumeWriter{}}).WriteResult("checkpoint"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v", err)
	}
}

func testResumeOperation(fill string, state resumeOperationState) resumeOperation {
	return resumeOperation{operationID: strings.Repeat(fill, 32), state: state}
}

type writerLogger struct {
	writer io.Writer
}

func (logger writerLogger) Logf(format string, args ...any) {
	if logger.writer != nil {
		_, _ = fmt.Fprintln(logger.writer, fmt.Sprintf(format, args...))
	}
}

type resumeTestApp struct {
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	resumeInventories  resumeStateInventoryOpener
	resumeConfirmation resumeConfirmationTerminal
}

func newResumeTestApp() (*resumeTestApp, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &resumeTestApp{
		stdout: stdout,
		stderr: stderr,
		stdin:  strings.NewReader(""),
	}, stdout, stderr
}

func (app *resumeTestApp) Run(ctx context.Context, args []string) Result {
	if len(args) == 0 || args[0] != "resume" {
		return ResultUsage
	}
	return app.runner().Run(ctx, args[1:])
}

func (app *resumeTestApp) parser() flagRequestParser {
	return flagRequestParser{
		logger: writerLogger{writer: app.stderr},
	}
}

func (app *resumeTestApp) runner() Runner {
	inventories := app.resumeInventories
	if inventories == nil {
		inventories = filesystemResumeStateInventoryOpener{}
	}
	confirmation := app.resumeConfirmation
	if confirmation == nil {
		confirmation = newStdioResumeConfirmationTerminal(app.stdin, app.stderr, app.stderr)
	}
	logger := writerLogger{writer: app.stderr}
	return newRunner(resumeDependencies{
		inventories:  inventories,
		confirmation: confirmation,
		parser:       app.parser(),
		renderer:     textRenderer{},
		output: streamResumeOutput{
			result: app.stdout,
			usage:  app.stderr,
		},
		logger: logger,
	})
}

type fakeResumeStateInventoryOpener struct {
	inventory resumeStateInventory
	err       error
	calls     int
	rootPath  string
}

func (opener *fakeResumeStateInventoryOpener) OpenResumeStateInventory(
	_ context.Context,
	rootPath string,
) (resumeStateInventory, error) {
	opener.calls++
	opener.rootPath = rootPath
	return opener.inventory, opener.err
}

type fakeResumeStateInventory struct {
	snapshot      resumeInventorySnapshot
	snapshotErr   error
	discardReport resumeDiscardReport
	discardErr    error
	discardCalls  int
	discardIndex  int
}

func (inventory *fakeResumeStateInventory) Snapshot() (resumeInventorySnapshot, error) {
	return inventory.snapshot.clone(), inventory.snapshotErr
}

func (inventory *fakeResumeStateInventory) Discard(
	_ context.Context,
	index int,
) (resumeDiscardReport, error) {
	inventory.discardCalls++
	inventory.discardIndex = index
	return inventory.discardReport, inventory.discardErr
}

type fakeResumeConfirmationTerminal struct {
	interactive bool
	line        string
	err         error
	calls       int
	prompt      string
}

func (terminal *fakeResumeConfirmationTerminal) Interactive() bool {
	return terminal.interactive
}

func (terminal *fakeResumeConfirmationTerminal) ReadLine(
	_ context.Context,
	prompt string,
) (string, error) {
	terminal.calls++
	terminal.prompt = prompt
	return terminal.line, terminal.err
}

type shortResumeWriter struct{}

func (shortResumeWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}
