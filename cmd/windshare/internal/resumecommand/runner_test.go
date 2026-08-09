package resumecommand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestResumeParsersAcceptOnlyAnOutputRootAndOneLiveItem(t *testing.T) {
	root := t.TempDir()
	app, _, _ := newResumeTestApp()

	request, code := app.parseResumeRootRequest("resume list", []string{"-o", root})
	if code != ResultOK || request.rootPath != root {
		t.Fatalf("list request=%+v code=%d", request, code)
	}
	discard, code := app.parseResumeDiscardRequest([]string{"--item", "2", "-o", root})
	if code != ResultOK || discard.rootPath != root || discard.itemNumber != 2 {
		t.Fatalf("discard request=%+v code=%d", discard, code)
	}

	for name, args := range map[string][]string{
		"missing root":       {"--item", "1"},
		"missing item":       {"-o", root},
		"zero item":          {"-o", root, "--item", "0"},
		"noncanonical item":  {"-o", root, "--item", "01"},
		"bulk item syntax":   {"-o", root, "--item", "1,2"},
		"repeated item":      {"-o", root, "--item", "1", "--item", "2"},
		"internal path flag": {"-o", root, "--item", "1", "--path", "records/one"},
		"positional path":    {"-o", root, "--item", "1", "records/one"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, code := app.parseResumeDiscardRequest(args); code != ResultUsage {
				t.Fatalf("code=%d", code)
			}
		})
	}
	for name, args := range map[string][]string{
		"missing root": nil,
		"item handle":  {"-o", root, "--item", "1"},
		"positional":   {"-o", root, "records/one"},
	} {
		t.Run("list "+name, func(t *testing.T) {
			if _, code := app.parseResumeRootRequest("resume list", args); code != ResultUsage {
				t.Fatalf("code=%d", code)
			}
		})
	}
}

func TestResumeListUsesFreshInventoryAndReportsNeedsAttention(t *testing.T) {
	root := t.TempDir()
	t.Run("resumable", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{availableResumeItem()}}
		opener := &fakeResumeStateInventoryOpener{inventory: inventory}
		app, stdout, stderr := newResumeTestApp()
		app.resumeInventories = opener

		if code := app.Run(context.Background(), []string{"resume", "list", "-o", root}); code != ResultOK {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
		if opener.calls != 1 || opener.rootPath != root {
			t.Fatalf("opener=%+v inventory=%+v", opener, inventory)
		}
		for _, want := range []string{
			`resume_list_status="ready" items=1`,
			`resume_item=1 status="resumable"`,
			`operation_id="11111111111111111111111111111111"`,
			`phase=4 state_generation=3 expires_at_millis=4096`,
			`resumable=true discardable=true`,
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
			}
		}
	})

	t.Run("needs attention", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{attentionResumeItem()}}
		app, stdout, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}

		if code := app.Run(context.Background(), []string{"resume", "list", "-o", root}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		for _, want := range []string{
			`resume_list_status="needs-attention"`,
			`reason="cleanup-unknown"`,
			`operation_id="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`,
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
			}
		}
		if !strings.Contains(stderr.String(), "no deletion authority was used") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestResumeListReportsTypedBusyWithoutOpeningLegacyCleanup(t *testing.T) {
	root := t.TempDir()
	opener := &fakeResumeStateInventoryOpener{err: fmt.Errorf("intent lease: %w", osfs.ErrResumeStateBusy)}
	cleaner := &fakeLegacyResumeCleaner{}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = opener
	app.legacyResumeCleaner = cleaner

	if code := app.Run(context.Background(), []string{"resume", "list", "-o", root}); code != ResultFailure {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stdout.String(), `resume_list_status="busy"`) ||
		!strings.Contains(stderr.String(), "authority is busy") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if cleaner.calls != 0 {
		t.Fatalf("legacy cleaner calls=%d", cleaner.calls)
	}
}

func TestResumeDiscardRequiresExactTerminalIntentForTheFreshOperation(t *testing.T) {
	root := t.TempDir()
	inventory := &fakeResumeStateInventory{
		items:         []resumeStateItem{availableResumeItem()},
		discardReport: settledResumeDiscardReport(),
	}
	terminal := &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = terminal

	if code := app.Run(context.Background(), []string{
		"resume", "discard", "-o", root, "--item", "1",
	}); code != ResultOK {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if inventory.discardCalls != 1 || inventory.discardIndex != 0 {
		t.Fatalf("inventory=%+v", inventory)
	}
	if terminal.calls != 1 || !strings.Contains(terminal.prompt, `resume_item=1 status="resumable"`) ||
		!strings.Contains(terminal.prompt, `Type "discard 1" exactly`) ||
		!strings.Contains(terminal.prompt, "Published files are preserved") {
		t.Fatalf("terminal=%+v", terminal)
	}
	for _, want := range []string{
		`resume_discard_status="settled"`,
		`operation_id="11111111111111111111111111111111"`,
		`phase=18 state_generation=4 resumable=false`,
		`published_files="preserved"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
		}
	}
}

func TestResumeDiscardRejectsRedirectedOrInexactConfirmation(t *testing.T) {
	root := t.TempDir()
	tests := map[string]struct {
		terminal *fakeResumeConfirmationTerminal
		status   string
	}{
		"redirected": {
			terminal: &fakeResumeConfirmationTerminal{interactive: false, line: "discard 1"},
			status:   resumeConfirmationStatus,
		},
		"trailing space": {
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1 "},
			status:   resumeNotConfirmedStatus,
		},
		"wrong case": {
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "Discard 1"},
			status:   resumeNotConfirmedStatus,
		},
		"different ordinal": {
			terminal: &fakeResumeConfirmationTerminal{interactive: true, line: "discard 01"},
			status:   resumeNotConfirmedStatus,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			inventory := &fakeResumeStateInventory{items: []resumeStateItem{availableResumeItem()}}
			app, stdout, _ := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
			app.resumeConfirmation = test.terminal

			if code := app.Run(context.Background(), []string{
				"resume", "discard", "-o", root, "--item", "1",
			}); code != ResultFailure {
				t.Fatalf("code=%d", code)
			}
			if inventory.discardCalls != 0 {
				t.Fatalf("inventory=%+v", inventory)
			}
			if !strings.Contains(stdout.String(), fmt.Sprintf(`resume_discard_status=%q`, test.status)) {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if !test.terminal.interactive && test.terminal.calls != 0 {
				t.Fatalf("redirected terminal read calls=%d", test.terminal.calls)
			}
		})
	}
}

func TestResumeDiscardReportsBusyAndNeedsAttentionAsClosedOutcomes(t *testing.T) {
	root := t.TempDir()
	t.Run("busy", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{
			items:      []resumeStateItem{availableResumeItem()},
			discardErr: fmt.Errorf("runtime lease: %w", osfs.ErrResumeStateBusy),
		}
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}

		if code := app.Run(context.Background(), []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		for _, want := range []string{
			`resume_discard_status="busy"`, `phase="discard"`, `published_files="preserved"`,
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout=%q missing=%q", stdout.String(), want)
			}
		}
		if inventory.discardCalls != 1 {
			t.Fatalf("inventory=%+v", inventory)
		}
	})

	t.Run("needs attention", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{
			items: []resumeStateItem{attentionResumeItem()},
			discardReport: resumeDiscardReport{
				status:      resumeDiscardStatusNeedsAttention,
				operationID: strings.Repeat("a", 32), phase: 20, stateGeneration: 5,
				attention: []resumeStateAttention{{
					reason: "cleanup-unknown", operationID: strings.Repeat("a", 32),
				}},
			},
		}
		app, stdout, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}

		if code := app.Run(context.Background(), []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(stdout.String(), `resume_discard_status="needs-attention"`) ||
			!strings.Contains(stdout.String(), `reason="cleanup-unknown"`) ||
			!strings.Contains(stdout.String(), `published_files="preserved"`) ||
			!strings.Contains(stderr.String(), "uncertain and published objects were preserved") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("published terminal record remains truthful", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{
			items: []resumeStateItem{availableResumeItem()},
			discardReport: resumeDiscardReport{
				status: resumeDiscardStatusSettled, operationID: strings.Repeat("1", 32),
				phase: 14, stateGeneration: 4,
			},
		}
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
		if code := app.Run(context.Background(), []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); code != ResultOK {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(stdout.String(), `resume_discard_status="settled"`) ||
			!strings.Contains(stdout.String(), `phase=14 state_generation=4 resumable=false`) {
			t.Fatalf("stdout=%q", stdout.String())
		}
	})
}

func TestResumeDiscardValidatesCurrentOrdinalBeforePrompt(t *testing.T) {
	inventory := &fakeResumeStateInventory{items: []resumeStateItem{availableResumeItem()}}
	terminal := &fakeResumeConfirmationTerminal{interactive: true, line: "discard 2"}
	app, _, _ := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = terminal

	if code := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", "2",
	}); code != ResultUsage {
		t.Fatalf("code=%d", code)
	}
	if terminal.calls != 0 || inventory.discardCalls != 0 {
		t.Fatalf("terminal=%+v inventory=%+v", terminal, inventory)
	}
}

func TestResumeDiscardRefusesAttentionOnlyInventoryEvidence(t *testing.T) {
	operationID := strings.Repeat("c", 32)
	inventory := &fakeResumeStateInventory{items: []resumeStateItem{{
		status: resumeItemStatusNeedsAttention, operationID: operationID,
		attention: []resumeStateAttention{{
			reason: "target-ownership-unknown", operationID: operationID,
		}},
	}}}
	terminal := &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
	app, _, stderr := newResumeTestApp()
	app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
	app.resumeConfirmation = terminal

	if code := app.Run(context.Background(), []string{
		"resume", "discard", "-o", t.TempDir(), "--item", "1",
	}); code != ResultFailure {
		t.Fatalf("code=%d", code)
	}
	if terminal.calls != 0 || inventory.discardCalls != 0 ||
		!strings.Contains(stderr.String(), "no mutation authority") {
		t.Fatalf("terminal=%+v inventory=%+v stderr=%q", terminal, inventory, stderr.String())
	}
}

func TestResumeLegacyCleanupCannotSubstituteForCurrentDiscard(t *testing.T) {
	root := t.TempDir()
	cleaner := &fakeLegacyResumeCleaner{report: osfs.CheckpointCleanupReport{
		Status: osfs.CheckpointCleanupStatusComplete, Complete: true,
		Scanned: 2, Removed: 1,
	}}
	opener := &fakeResumeStateInventoryOpener{err: errors.New("current authority must remain unused")}
	app, stdout, stderr := newResumeTestApp()
	app.resumeInventories = opener
	app.legacyResumeCleaner = cleaner

	if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ResultOK {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if cleaner.calls != 1 || cleaner.rootPath != root || opener.calls != 0 {
		t.Fatalf("cleaner=%+v opener=%+v", cleaner, opener)
	}
	if !strings.Contains(stdout.String(), `legacy_cleanup_status="complete"`) ||
		strings.Contains(stdout.String(), "resume_discard_status") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestResumeLegacyCleanupReportsAttentionBusyAndOutputFailures(t *testing.T) {
	root := t.TempDir()
	t.Run("attention", func(t *testing.T) {
		app, stdout, stderr := newResumeTestApp()
		app.legacyResumeCleaner = &fakeLegacyResumeCleaner{report: osfs.CheckpointCleanupReport{
			Status:    osfs.CheckpointCleanupStatusNeedsAttention,
			Attention: []string{"ownership\nmarker"},
		}}
		if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(stdout.String(), `legacy_attention="ownership\nmarker"`) ||
			strings.Contains(stdout.String(), "ownership\nmarker\n") ||
			!strings.Contains(stderr.String(), "legacy resume state still needs attention") {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})

	t.Run("busy", func(t *testing.T) {
		app, stdout, _ := newResumeTestApp()
		app.legacyResumeCleaner = &fakeLegacyResumeCleaner{err: osfs.ErrCheckpointCleanerBusy}
		if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		if stdout.String() != "legacy_cleanup_status=\"busy\"\n" {
			t.Fatalf("stdout=%q", stdout.String())
		}
	})

	t.Run("empty report", func(t *testing.T) {
		app, _, stderr := newResumeTestApp()
		app.legacyResumeCleaner = &fakeLegacyResumeCleaner{}
		if code := app.Run(context.Background(), []string{"resume", "cleanup", "-o", root}); code != ResultFailure {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(stderr.String(), "legacy cleanup report is empty") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	if err := (&resumeTestApp{Stdout: shortResumeWriter{}}).writeResumeOutput("checkpoint"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v", err)
	}
}

func TestFilesystemLegacyCleanerForwardsOnlyTheOwnedRoot(t *testing.T) {
	root := t.TempDir()
	var observed osfs.FilesystemResumeRoot
	cleaner := filesystemLegacyResumeCleaner{clean: func(
		_ context.Context,
		requested osfs.FilesystemResumeRoot,
	) (osfs.CheckpointCleanupReport, error) {
		observed = requested
		return osfs.CheckpointCleanupReport{
			Status: osfs.CheckpointCleanupStatusComplete, Complete: true,
		}, nil
	}}
	report, err := cleaner.CleanLegacy(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if observed.RootPath != root || !report.Complete {
		t.Fatalf("observed=%+v report=%+v", observed, report)
	}
}

func TestStdioResumeConfirmationRejectsRedirectedInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	terminal := newStdioResumeConfirmationTerminal(reader, writer, writer)
	if terminal.Interactive() {
		t.Fatal("pipe-backed confirmation was treated as a terminal")
	}
}

func TestStdioResumeConfirmationReadsOneExactTerminalLine(t *testing.T) {
	output := &bytes.Buffer{}
	terminal := stdioResumeConfirmationTerminal{
		input: strings.NewReader("discard 3\r\nignored\n"), output: output, interactive: true,
	}
	line, err := terminal.ReadLine(context.Background(), "confirm: ")
	if err != nil {
		t.Fatal(err)
	}
	if line != "discard 3" || output.String() != "confirm: " {
		t.Fatalf("line=%q output=%q", line, output.String())
	}
}

func TestResumeSurfaceExposesNoCompatibilityOrSessionDeletionCommands(t *testing.T) {
	for _, action := range []string{"status", "delete", "purge", "cancel", "pause", "complete"} {
		app, _, stderr := newResumeTestApp()
		if code := app.Run(context.Background(), []string{"resume", action}); code != ResultUsage {
			t.Fatalf("action=%q code=%d", action, code)
		}
		if !strings.Contains(stderr.String(), "unknown action") {
			t.Fatalf("action=%q stderr=%q", action, stderr.String())
		}
	}
	app, _, stderr := newResumeTestApp()
	if code := app.Run(context.Background(), []string{"resume", "help"}); code != ResultOK {
		t.Fatalf("code=%d", code)
	}
	help := stderr.String()
	for _, want := range []string{"resume list", "resume discard", "resume cleanup", "published files are always preserved", "legacy-state"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help=%q missing=%q", help, want)
		}
	}
	for _, forbidden := range []string{"--path", "--all", "PauseJob", "CompleteJob"} {
		if strings.Contains(help, forbidden) {
			t.Fatalf("help=%q exposed=%q", help, forbidden)
		}
	}
}

func availableResumeItem() resumeStateItem {
	return resumeStateItem{
		status: resumeItemStatusResumable, operationID: strings.Repeat("1", 32),
		intentDigest: strings.Repeat("2", 64), phase: 4, stateGeneration: 3,
		expiresAtMillis: 4096, successCount: 2, failureCount: 1,
		resumable: true, discardable: true,
	}
}

func attentionResumeItem() resumeStateItem {
	return resumeStateItem{
		status: resumeItemStatusNeedsAttention, operationID: strings.Repeat("a", 32),
		intentDigest: strings.Repeat("b", 64), phase: 20, stateGeneration: 4,
		discardable: true,
		attention: []resumeStateAttention{{
			reason: "cleanup-unknown", operationID: strings.Repeat("a", 32),
		}},
	}
}

func settledResumeDiscardReport() resumeDiscardReport {
	return resumeDiscardReport{
		status: resumeDiscardStatusSettled, operationID: strings.Repeat("1", 32),
		phase: 18, stateGeneration: 4,
	}
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
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	resumeInventories   resumeStateInventoryOpener
	resumeConfirmation  resumeConfirmationTerminal
	legacyResumeCleaner legacyResumeCleanupRunner
}

func newResumeTestApp() (*resumeTestApp, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &resumeTestApp{
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}, stdout, stderr
}

func (app *resumeTestApp) Run(ctx context.Context, args []string) Result {
	if len(args) == 0 || args[0] != "resume" {
		return ResultUsage
	}
	return app.runner().Run(ctx, args[1:])
}

func (app *resumeTestApp) parseResumeRootRequest(
	action string,
	args []string,
) (resumeRootRequest, Result) {
	request, valid := app.parser().ParseRoot(action, args)
	if !valid {
		return resumeRootRequest{}, ResultUsage
	}
	return request, ResultOK
}

func (app *resumeTestApp) parseResumeDiscardRequest(args []string) (resumeDiscardRequest, Result) {
	request, valid := app.parser().ParseDiscard(args)
	if !valid {
		return resumeDiscardRequest{}, ResultUsage
	}
	return request, ResultOK
}

func (app *resumeTestApp) writeResumeOutput(value string) error {
	return (streamResumeOutput{result: app.Stdout}).WriteResult(value)
}

func (app *resumeTestApp) parser() flagRequestParser {
	return flagRequestParser{
		output: app.Stderr,
		logger: writerLogger{writer: app.Stderr},
	}
}

func (app *resumeTestApp) runner() Runner {
	inventories := app.resumeInventories
	if inventories == nil {
		inventories = filesystemResumeStateInventoryOpener{}
	}
	legacy := app.legacyResumeCleaner
	if legacy == nil {
		legacy = filesystemLegacyResumeCleaner{}
	}
	confirmation := app.resumeConfirmation
	if confirmation == nil {
		confirmation = newStdioResumeConfirmationTerminal(app.Stdin, app.Stderr, app.Stderr)
	}
	logger := writerLogger{writer: app.Stderr}
	return newRunner(resumeDependencies{
		inventories:  inventories,
		legacy:       legacy,
		confirmation: confirmation,
		parser:       app.parser(),
		renderer:     textRenderer{},
		output: streamResumeOutput{
			result: app.Stdout,
			usage:  app.Stderr,
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
	items         []resumeStateItem
	itemsErr      error
	discardReport resumeDiscardReport
	discardErr    error
	discardCalls  int
	discardIndex  int
}

func (inventory *fakeResumeStateInventory) Items() ([]resumeStateItem, error) {
	items := make([]resumeStateItem, len(inventory.items))
	copy(items, inventory.items)
	return items, inventory.itemsErr
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

type fakeLegacyResumeCleaner struct {
	report   osfs.CheckpointCleanupReport
	err      error
	calls    int
	rootPath string
}

func (cleaner *fakeLegacyResumeCleaner) CleanLegacy(
	_ context.Context,
	rootPath string,
) (osfs.CheckpointCleanupReport, error) {
	cleaner.calls++
	cleaner.rootPath = rootPath
	return cleaner.report, cleaner.err
}

type shortResumeWriter struct{}

func (shortResumeWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}
