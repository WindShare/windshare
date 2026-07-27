package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestResumeRequestParsingKeepsDestructiveSurfaceNarrow(t *testing.T) {
	root := t.TempDir()
	app, _, stderr := newResumeTestApp(strings.NewReader(""), nil)

	list, code := app.parseResumeListRequest([]string{"-o", root})
	if code != ExitOK || list.rootPath != root {
		t.Fatalf("list=%+v code=%d", list, code)
	}
	defaultList, code := app.parseResumeListRequest(nil)
	if code != ExitOK || !filepath.IsAbs(defaultList.rootPath) {
		t.Fatalf("default list=%+v code=%d", defaultList, code)
	}

	discard, code := app.parseResumeDiscardRequest([]string{"-o", root, "--item", "2"})
	if code != ExitOK || discard.rootPath != root || discard.item != 2 {
		t.Fatalf("discard=%+v code=%d", discard, code)
	}

	invalid := []struct {
		name string
		args []string
	}{
		{name: "list positional", args: []string{"list", "extra"}},
		{name: "list empty root", args: []string{"list", "-o", ""}},
		{name: "discard missing root", args: []string{"discard", "--item", "1"}},
		{name: "discard missing item", args: []string{"discard", "-o", root}},
		{name: "discard zero", args: []string{"discard", "-o", root, "--item", "0"}},
		{name: "discard negative", args: []string{"discard", "-o", root, "--item", "-1"}},
		{name: "discard leading zero", args: []string{"discard", "-o", root, "--item", "01"}},
		{name: "discard leading plus", args: []string{"discard", "-o", root, "--item", "+1"}},
		{name: "discard non decimal", args: []string{"discard", "-o", root, "--item", "one"}},
		{name: "discard overflow", args: []string{"discard", "-o", root, "--item", "18446744073709551616"}},
		{name: "discard positional", args: []string{"discard", "-o", root, "--item", "1", "extra"}},
		{name: "discard all forbidden", args: []string{"discard", "-o", root, "--item", "1", "--all"}},
		{name: "discard yes forbidden", args: []string{"discard", "-o", root, "--item", "1", "--yes"}},
		{name: "discard token forbidden", args: []string{"discard", "-o", root, "--item", "1", "--reference", "opaque"}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			stderr.Reset()
			switch test.args[0] {
			case "list":
				if _, got := app.parseResumeListRequest(test.args[1:]); got != ExitUsage {
					t.Fatalf("code=%d", got)
				}
			case "discard":
				if _, got := app.parseResumeDiscardRequest(test.args[1:]); got != ExitUsage {
					t.Fatalf("code=%d", got)
				}
			}
		})
	}
}

func TestResumeCommandDispatchAndHelp(t *testing.T) {
	app, _, stderr := newResumeTestApp(strings.NewReader(""), nil)
	for _, test := range []struct {
		args []string
		code int
		text string
	}{
		{args: []string{"resume"}, code: ExitUsage, text: "exactly one action"},
		{args: []string{"resume", "unknown"}, code: ExitUsage, text: "unknown action"},
		{args: []string{"resume", "help"}, code: ExitOK, text: "resume discard"},
	} {
		stderr.Reset()
		if got := app.Run(context.Background(), test.args); got != test.code {
			t.Fatalf("args=%v code=%d want=%d", test.args, got, test.code)
		}
		if !strings.Contains(stderr.String(), test.text) {
			t.Fatalf("args=%v stderr=%q", test.args, stderr.String())
		}
	}
}

func TestResumeRenderingQuotesUntrustedAttentionMetadata(t *testing.T) {
	items := []resumeStateItem{
		{
			number: 1, kind: osfs.ResumeStateNeedsAttention,
			lifecycle:    osfs.ResumeSessionPausedNeedsAttention,
			resumeIntent: "intent", sessionID: "session", fileRecords: 2, allocatedBytes: 9,
			attention: []osfs.ResumeAttention{{
				Scope: osfs.ResumeAttentionFile,
				Code:  "line\nnext", State: "tab\tstate", Detail: "quote\"\x1b[31m",
			}},
		},
	}
	rendered, err := renderResumeStateItems(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, safe := range []string{
		`kind="needs-attention"`,
		`lifecycle="paused-needs-attention"`,
		`scope="file"`,
		`code="line\nnext"`,
		`state="tab\tstate"`,
		`detail="quote\"\x1b[31m"`,
	} {
		if !strings.Contains(rendered, safe) {
			t.Fatalf("rendered=%q missing=%q", rendered, safe)
		}
	}
	if strings.ContainsRune(rendered, '\x1b') || strings.Contains(rendered, "line\nnext") {
		t.Fatalf("untrusted control characters escaped the quoted field: %q", rendered)
	}
}

func TestResumeValueProjectionIsExhaustive(t *testing.T) {
	kinds := map[osfs.ResumeStateKind]string{
		osfs.ResumeStateRecoverable:     "recoverable",
		osfs.ResumeStateNeedsAttention:  "needs-attention",
		osfs.ResumeStateLegacyUntrusted: "legacy-untrusted",
		osfs.ResumeStateOpaqueUnsafe:    "opaque-unsafe",
		0:                               "unknown",
	}
	for value, want := range kinds {
		if got := resumeStateKindName(value); got != want {
			t.Fatalf("kind %d=%q want=%q", value, got, want)
		}
	}
	lifecycles := []osfs.ResumeSessionLifecycle{
		osfs.ResumeSessionActive,
		osfs.ResumeSessionPausing,
		osfs.ResumeSessionPaused,
		osfs.ResumeSessionPausedNeedsAttention,
		osfs.ResumeSessionCompleting,
		osfs.ResumeSessionDiscarding,
	}
	for _, value := range lifecycles {
		if got := resumeLifecycleName(value); got != value.String() {
			t.Fatalf("lifecycle %d=%q want=%q", value, got, value.String())
		}
	}
	if got := resumeLifecycleName(0); got != "unknown" {
		t.Fatalf("zero lifecycle=%q", got)
	}
	scopes := map[osfs.ResumeAttentionScope]string{
		osfs.ResumeAttentionFile:   "file",
		osfs.ResumeAttentionIntent: "intent",
		osfs.ResumeAttentionRoot:   "root",
		osfs.ResumeAttentionLegacy: "legacy",
		0:                          "unknown",
	}
	for value, want := range scopes {
		if got := resumeAttentionScopeName(value); got != want {
			t.Fatalf("scope %d=%q want=%q", value, got, want)
		}
	}
}

func TestFilesystemResumeInventoryKeepsCoreAuthorityInsideAdapter(t *testing.T) {
	internal := &osfs.ResumeStateInventory{}
	discardCalls := 0
	inventory := newFilesystemResumeStateInventory(
		internal,
		[]osfs.ResumeStateSummary{{
			Lifecycle:      osfs.ResumeSessionPaused,
			FileRecords:    3,
			AllocatedBytes: 11,
			Attention:      []osfs.ResumeAttention{{Scope: osfs.ResumeAttentionIntent, Code: "attention"}},
		}, {FileRecords: 4}},
		func(context.Context, osfs.ResumeStateRef) (osfs.DiscardSettlement, error) {
			discardCalls++
			return osfs.DiscardSettlement{Kind: osfs.Discarded, RemovedBytes: 11}, nil
		},
	)
	items := inventory.Items()
	if len(items) != 2 || items[0].number != 1 || items[0].fileRecords != 3 ||
		items[0].allocatedBytes != 11 || items[0].resumeIntent != "-" || items[0].sessionID != "-" ||
		len(items[0].attention) != 1 {
		t.Fatalf("items=%+v", items)
	}
	if items[0].authority == nil || items[1].authority == nil || items[0].authority == items[1].authority {
		t.Fatal("inventory items did not receive distinct live authorities")
	}
	items[0].attention[0].Code = "mutated"
	if inventory.items[0].attention[0].Code != "attention" {
		t.Fatal("projected attention aliased the captured core summary")
	}
	if _, err := inventory.Discard(context.Background(), resumeStateItem{}); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("zero item error=%v", err)
	}
	if _, err := inventory.Discard(
		context.Background(), resumeStateItem{number: 3, authority: &resumeStateAuthority{}},
	); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("out-of-range item error=%v", err)
	}
	forged := items[0]
	forged.authority = items[1].authority
	if _, err := inventory.Discard(context.Background(), forged); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("cross-item authority error=%v", err)
	}
	settlement, err := inventory.Discard(context.Background(), items[0])
	if err != nil || settlement.Kind != osfs.Discarded || discardCalls != 1 {
		t.Fatalf("settlement=%+v calls=%d err=%v", settlement, discardCalls, err)
	}
	if _, err := inventory.Discard(context.Background(), items[0]); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("second discard error=%v", err)
	}
	if discardCalls != 1 {
		t.Fatalf("discard backend calls=%d after stale retry", discardCalls)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatalf("idempotent adapter close: %v", err)
	}
	if items := inventory.Items(); items != nil {
		t.Fatalf("closed items=%v", items)
	}
	if _, err := inventory.Discard(context.Background(), items[0]); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("post-close discard error=%v", err)
	}

	var absent *filesystemResumeStateInventory
	if absent.Items() != nil {
		t.Fatal("nil inventory exposed items")
	}
	if _, err := absent.Discard(context.Background(), resumeStateItem{number: 1}); !errors.Is(err, errResumeItemUnavailable) {
		t.Fatalf("nil discard error=%v", err)
	}
	if err := absent.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}
	if got := resumeIdentity([]byte{0, 1}); got != "0001" {
		t.Fatalf("identity=%q", got)
	}
}

func TestFilesystemResumeSourceProjectsBackendInventory(t *testing.T) {
	if _, ok := (&App{}).resumeStates().(filesystemResumeStateSource); !ok {
		t.Fatal("default resume source is not the filesystem adapter")
	}
	root := t.TempDir()
	if _, err := (filesystemResumeStateSource{}).Open(
		context.Background(), filepath.Join(root, "missing"),
	); err == nil {
		t.Fatal("missing native root unexpectedly produced an inventory")
	}
	var observed osfs.FilesystemResumeRoot
	source := filesystemResumeStateSource{list: func(
		_ context.Context,
		requested osfs.FilesystemResumeRoot,
	) (*osfs.ResumeStateInventory, error) {
		observed = requested
		return &osfs.ResumeStateInventory{}, nil
	}}
	inventory, err := source.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if observed.RootPath != root {
		t.Fatalf("root=%q", observed.RootPath)
	}
	if items := inventory.Items(); len(items) != 0 {
		t.Fatalf("items=%v", items)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("list failed")
	for _, test := range []struct {
		name string
		list resumeStateListFunc
	}{
		{
			name: "backend error",
			list: func(context.Context, osfs.FilesystemResumeRoot) (*osfs.ResumeStateInventory, error) {
				return nil, failure
			},
		},
		{
			name: "nil inventory",
			list: func(context.Context, osfs.FilesystemResumeRoot) (*osfs.ResumeStateInventory, error) {
				return nil, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := (filesystemResumeStateSource{list: test.list}).Open(
				context.Background(), root,
			); err == nil {
				t.Fatal("missing source error")
			}
		})
	}
}

func TestResumeListPublishesOnlyAfterInventoryRelease(t *testing.T) {
	root := t.TempDir()
	inventory := &fakeResumeInventory{items: []resumeStateItem{
		{number: 1, kind: osfs.ResumeStateRecoverable, lifecycle: osfs.ResumeSessionPaused},
		{number: 2, kind: osfs.ResumeStateOpaqueUnsafe},
	}}
	source := &fakeResumeSource{inventory: inventory}
	app, stdout, _ := newResumeTestApp(strings.NewReader(""), source)
	if code := app.Run(context.Background(), []string{"resume", "list", "-o", root}); code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if inventory.closeCalls != 1 {
		t.Fatalf("close calls=%d", inventory.closeCalls)
	}
	if len(source.roots) != 1 || source.roots[0] != root {
		t.Fatalf("roots=%v", source.roots)
	}
	output := stdout.String()
	if first, second := strings.Index(output, "item=1"), strings.Index(output, "item=2"); first < 0 || second <= first {
		t.Fatalf("output order=%q", output)
	}
}

func TestResumeListFailurePathsCloseExactlyOnce(t *testing.T) {
	root := t.TempDir()
	failure := errors.New("injected failure")
	for _, test := range []struct {
		name      string
		sourceErr error
		closeErr  error
		writer    io.Writer
		items     []resumeStateItem
		wantClose int
		wantOut   string
	}{
		{name: "empty", writer: &bytes.Buffer{}, wantClose: 1, wantOut: "No recovery state found."},
		{name: "source", sourceErr: failure, writer: &bytes.Buffer{}},
		{name: "nil inventory", writer: &bytes.Buffer{}},
		{name: "close", closeErr: failure, writer: &bytes.Buffer{}, wantClose: 1},
		{name: "writer", writer: resumeErrorWriter{err: failure}, wantClose: 1},
		{name: "short writer", writer: resumeShortWriter{}, wantClose: 1},
		{name: "missing writer", writer: nil, wantClose: 1},
		{
			name: "render and deferred close", writer: &bytes.Buffer{}, closeErr: failure,
			items: []resumeStateItem{{number: 0}}, wantClose: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := &fakeResumeInventory{items: test.items, closeErr: test.closeErr}
			var provided resumeStateInventory = inventory
			if test.name == "nil inventory" {
				provided = nil
			}
			source := &fakeResumeSource{inventory: provided, err: test.sourceErr}
			stderr := &bytes.Buffer{}
			app := &App{Stdout: test.writer, Stderr: stderr, Stdin: strings.NewReader(""), resumeSource: source}
			code := app.Run(context.Background(), []string{"resume", "list", "-o", root})
			if test.name == "empty" {
				if code != ExitOK || !strings.Contains(test.writer.(*bytes.Buffer).String(), test.wantOut) {
					t.Fatalf("code=%d output=%q", code, test.writer.(*bytes.Buffer).String())
				}
			} else if code != ExitFailure {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if inventory.closeCalls != test.wantClose {
				t.Fatalf("close calls=%d want=%d", inventory.closeCalls, test.wantClose)
			}
			if test.name == "close" && test.writer.(*bytes.Buffer).Len() != 0 {
				t.Fatalf("close failure published success output: %q", test.writer.(*bytes.Buffer).String())
			}
		})
	}
}

func TestResumeDiscardUsesFreshPreviewAndExactItem(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name       string
		input      string
		settlement osfs.DiscardSettlement
		wantText   string
	}{
		{
			name: "discarded LF", input: "discard 2\n",
			settlement: osfs.DiscardSettlement{Kind: osfs.Discarded, RemovedBytes: 77},
			wantText:   "Discarded recovery item 2 (77 internal allocated bytes).",
		},
		{
			name: "discarded CRLF", input: "discard 2\r\n",
			settlement: osfs.DiscardSettlement{Kind: osfs.Discarded, RemovedBytes: 2},
			wantText:   "Discarded recovery item 2",
		},
		{
			name: "discarded EOF", input: "discard 2",
			settlement: osfs.DiscardSettlement{Kind: osfs.Discarded, RemovedBytes: 3},
			wantText:   "Discarded recovery item 2",
		},
		{
			name: "already absent", input: "discard 2\n",
			settlement: osfs.DiscardSettlement{Kind: osfs.DiscardAlreadyAbsent},
			wantText:   "Recovery item 2 was already absent.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := &fakeResumeInventory{
				items: []resumeStateItem{
					{number: 1, kind: osfs.ResumeStateRecoverable},
					{number: 2, kind: osfs.ResumeStateNeedsAttention, allocatedBytes: 77},
				},
				settlement: test.settlement,
			}
			app, stdout, stderr := newResumeTestApp(strings.NewReader(test.input), &fakeResumeSource{inventory: inventory})
			code := app.Run(context.Background(), []string{"resume", "discard", "-o", root, "--item", "2"})
			if code != ExitOK {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if len(inventory.discards) != 1 || inventory.discards[0] != 2 {
				t.Fatalf("discards=%v", inventory.discards)
			}
			if inventory.closeCalls != 1 {
				t.Fatalf("close calls=%d", inventory.closeCalls)
			}
			if output := stdout.String(); strings.Contains(output, "item=") || !strings.Contains(output, test.wantText) {
				t.Fatalf("stdout=%q", output)
			}
			if prompt := stderr.String(); strings.Contains(prompt, "item=1") ||
				!strings.Contains(prompt, "item=2") || !strings.Contains(prompt, `Type "discard 2"`) {
				t.Fatalf("prompt=%q", stderr.String())
			}
		})
	}
}

func TestResumeDiscardKeepsAuthorityLiveThroughConfirmation(t *testing.T) {
	root := t.TempDir()
	events := []string{}
	authority := &resumeStateAuthority{}
	inventory := &fakeResumeInventory{
		items:        []resumeStateItem{{number: 1, authority: authority}},
		settlement:   osfs.DiscardSettlement{Kind: osfs.Discarded},
		sharedEvents: &events,
	}
	source := &fakeResumeSource{inventory: inventory, sharedEvents: &events}
	stdout := &resumeSequenceWriter{events: &events, labels: []string{"settlement"}}
	stderr := &resumeSequenceWriter{events: &events, labels: []string{"preview", "prompt"}}
	stdin := &resumeSequenceReader{events: &events, reader: strings.NewReader("discard 1\n")}
	app := &App{
		Stdout: stdout, Stderr: stderr, Stdin: stdin, resumeSource: source,
		resumeInteractive: alwaysInteractiveResumeStreams,
	}
	if code := app.Run(
		context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
	); code != ExitOK {
		t.Fatalf("code=%d events=%v", code, events)
	}
	want := "open,items,preview,prompt,confirm,discard,close,settlement"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("events=%q want=%q", got, want)
	}
	if len(inventory.authorities) != 1 || inventory.authorities[0] != authority {
		t.Fatalf("discard authorities=%v want=%p", inventory.authorities, authority)
	}
}

func TestResumeDiscardRejectsConfirmationWithoutMutation(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name     string
		input    string
		wantCode int
	}{
		{name: "blank", input: "\n", wantCode: ExitUsage},
		{name: "wrong item", input: "discard 1\n", wantCode: ExitUsage},
		{name: "leading whitespace", input: " discard 2\n", wantCode: ExitUsage},
		{name: "trailing whitespace", input: "discard 2 \n", wantCode: ExitUsage},
		{name: "oversized", input: strings.Repeat("x", maxResumeDiscardConfirmationBytes) + "\n", wantCode: ExitFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 2}}}
			app, _, _ := newResumeTestApp(strings.NewReader(test.input), &fakeResumeSource{inventory: inventory})
			if code := app.Run(
				context.Background(), []string{"resume", "discard", "-o", root, "--item", "2"},
			); code != test.wantCode {
				t.Fatalf("code=%d want=%d", code, test.wantCode)
			}
			if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
				t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
			}
		})
	}
}

func TestResumeDiscardRejectsPipedConfirmationBeforeInventoryOpen(t *testing.T) {
	root := t.TempDir()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.WriteString(writer, "discard 1\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
	source := &fakeResumeSource{inventory: inventory}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr, Stdin: reader, resumeSource: source}
	if code := app.Run(
		context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
	); code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	if len(source.roots) != 0 || len(inventory.discards) != 0 || inventory.closeCalls != 0 {
		t.Fatalf("roots=%v discards=%v close=%d", source.roots, inventory.discards, inventory.closeCalls)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeDiscardRejectsRedirectedPromptBeforeInventoryOpen(t *testing.T) {
	root := t.TempDir()
	input := strings.NewReader("discard 1\n")
	redirectedPrompt := &bytes.Buffer{}
	inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
	source := &fakeResumeSource{inventory: inventory}
	probeCalls := 0
	app := &App{
		Stdout: &bytes.Buffer{}, Stderr: redirectedPrompt, Stdin: input, resumeSource: source,
		resumeInteractive: func(candidateInput io.Reader, candidatePrompt io.Writer) bool {
			probeCalls++
			inputIsInteractive := candidateInput == input
			promptIsInteractive := candidatePrompt != redirectedPrompt
			return inputIsInteractive && promptIsInteractive
		},
	}
	if code := app.Run(
		context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
	); code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	if probeCalls != 1 || len(source.roots) != 0 || len(inventory.discards) != 0 || inventory.closeCalls != 0 {
		t.Fatalf(
			"probe calls=%d roots=%v discards=%v close=%d",
			probeCalls, source.roots, inventory.discards, inventory.closeCalls,
		)
	}
	if !strings.Contains(redirectedPrompt.String(), "interactive terminal") {
		t.Fatalf("stderr=%q", redirectedPrompt.String())
	}
}

func TestResumeDiscardFailurePathsRemainFailSafe(t *testing.T) {
	root := t.TempDir()
	failure := errors.New("injected failure")
	t.Run("source error", func(t *testing.T) {
		app, _, _ := newResumeTestApp(
			strings.NewReader("discard 1\n"), &fakeResumeSource{err: failure},
		)
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("nil inventory", func(t *testing.T) {
		app, _, _ := newResumeTestApp(strings.NewReader("discard 1\n"), &fakeResumeSource{})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("missing current item", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		app, _, _ := newResumeTestApp(strings.NewReader("discard 2\n"), &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "2"},
		); code != ExitUsage {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("preview output", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		app := &App{
			Stdout: &bytes.Buffer{}, Stderr: resumeErrorWriter{err: failure}, Stdin: strings.NewReader("discard 1\n"),
			resumeSource: &fakeResumeSource{inventory: inventory}, resumeInteractive: alwaysInteractiveResumeStreams,
		}
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("preview short write", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		app := &App{
			Stdout: &bytes.Buffer{}, Stderr: resumeShortWriter{}, Stdin: strings.NewReader("discard 1\n"),
			resumeSource: &fakeResumeSource{inventory: inventory}, resumeInteractive: alwaysInteractiveResumeStreams,
		}
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("prompt output", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		prompt := &resumeNthWriteFailure{failAt: 2, err: failure}
		app := &App{
			Stdout: &bytes.Buffer{}, Stderr: prompt, Stdin: strings.NewReader("discard 1\n"),
			resumeSource: &fakeResumeSource{inventory: inventory}, resumeInteractive: alwaysInteractiveResumeStreams,
		}
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("prompt short write", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		prompt := &resumeNthShortWriter{shortAt: 2}
		app := &App{
			Stdout: &bytes.Buffer{}, Stderr: prompt, Stdin: strings.NewReader("discard 1\n"),
			resumeSource: &fakeResumeSource{inventory: inventory}, resumeInteractive: alwaysInteractiveResumeStreams,
		}
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("missing confirmation input", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		app, _, _ := newResumeTestApp(nil, &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("confirmation mismatch plus close failure", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}, closeErr: failure}
		app, _, _ := newResumeTestApp(strings.NewReader("no\n"), &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("discard error is not retried", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}, discardErr: failure}
		app, _, stderr := newResumeTestApp(strings.NewReader("discard 1\n"), &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 1 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
		if !strings.Contains(stderr.String(), "run resume list again") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("close error reports committed discard without success exit", func(t *testing.T) {
		inventory := &fakeResumeInventory{
			items: []resumeStateItem{{number: 1}}, settlement: osfs.DiscardSettlement{Kind: osfs.Discarded},
			closeErr: failure,
		}
		app, stdout, stderr := newResumeTestApp(strings.NewReader("discard 1\n"), &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if inventory.closeCalls != 1 || strings.Contains(stdout.String(), "Discarded recovery") {
			t.Fatalf("close=%d stdout=%q", inventory.closeCalls, stdout.String())
		}
		if !strings.Contains(stderr.String(), "item 1 was discarded") ||
			!strings.Contains(stderr.String(), "run resume list again") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("invalid settlement", func(t *testing.T) {
		inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
		app, _, _ := newResumeTestApp(strings.NewReader("discard 1\n"), &fakeResumeSource{inventory: inventory})
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 1 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
	})

	t.Run("settlement output failure reports completed mutation", func(t *testing.T) {
		inventory := &fakeResumeInventory{
			items:      []resumeStateItem{{number: 1}},
			settlement: osfs.DiscardSettlement{Kind: osfs.Discarded, RemovedBytes: 5},
		}
		stdout := &resumeNthWriteFailure{failAt: 1, err: failure}
		stderr := &bytes.Buffer{}
		app := &App{
			Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("discard 1\n"),
			resumeSource: &fakeResumeSource{inventory: inventory}, resumeInteractive: alwaysInteractiveResumeStreams,
		}
		if code := app.Run(
			context.Background(), []string{"resume", "discard", "-o", root, "--item", "1"},
		); code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if len(inventory.discards) != 1 || inventory.closeCalls != 1 {
			t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
		}
		if !strings.Contains(stderr.String(), "state was discarded but settlement output failed") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestResumeDiscardCancellationClosesInventoryWithoutMutation(t *testing.T) {
	root := t.TempDir()
	inventory := &fakeResumeInventory{items: []resumeStateItem{{number: 1}}}
	reader, writer := io.Pipe()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app, _, _ := newResumeTestApp(reader, &fakeResumeSource{inventory: inventory})
	if code := app.Run(ctx, []string{"resume", "discard", "-o", root, "--item", "1"}); code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	_ = writer.Close()
	if len(inventory.discards) != 0 || inventory.closeCalls != 1 {
		t.Fatalf("discards=%v close=%d", inventory.discards, inventory.closeCalls)
	}
}

func TestBoundedResumeConfirmationDoesNotNormalizeAuthority(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "LF", input: "discard 4\n", want: "discard 4"},
		{name: "CRLF", input: "discard 4\r\n", want: "discard 4"},
		{name: "EOF", input: "discard 4", want: "discard 4"},
		{name: "space retained", input: "discard 4 \n", want: "discard 4 "},
		{name: "oversized", input: strings.Repeat("x", maxResumeDiscardConfirmationBytes), wantErr: errResumeConfirmationTooLong},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readBoundedResumeConfirmation(strings.NewReader(test.input))
			if !errors.Is(err, test.wantErr) || got != test.want {
				t.Fatalf("line=%q err=%v want=%q/%v", got, err, test.want, test.wantErr)
			}
		})
	}
	failure := errors.New("read failed")
	if _, err := readBoundedResumeConfirmation(resumeErrorReader{err: failure}); !errors.Is(err, failure) {
		t.Fatalf("reader error=%v", err)
	}
}

func newResumeTestApp(
	stdin io.Reader,
	source resumeStateSource,
) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &App{
		Stdout: stdout, Stderr: stderr, Stdin: stdin, resumeSource: source,
		resumeInteractive: alwaysInteractiveResumeStreams,
	}, stdout, stderr
}

func alwaysInteractiveResumeStreams(io.Reader, io.Writer) bool { return true }

type fakeResumeSource struct {
	inventory    resumeStateInventory
	err          error
	roots        []string
	sharedEvents *[]string
}

func (source *fakeResumeSource) Open(_ context.Context, rootPath string) (resumeStateInventory, error) {
	if source.sharedEvents != nil {
		*source.sharedEvents = append(*source.sharedEvents, "open")
	}
	source.roots = append(source.roots, rootPath)
	if source.err != nil {
		return nil, source.err
	}
	return source.inventory, nil
}

type fakeResumeInventory struct {
	items        []resumeStateItem
	settlement   osfs.DiscardSettlement
	discardErr   error
	closeErr     error
	discards     []uint64
	authorities  []*resumeStateAuthority
	events       []string
	sharedEvents *[]string
	closeCalls   int
	closed       bool
}

func (inventory *fakeResumeInventory) Items() []resumeStateItem {
	inventory.record("items")
	if inventory.closed {
		return nil
	}
	result := append([]resumeStateItem(nil), inventory.items...)
	for index := range result {
		result[index].attention = append([]osfs.ResumeAttention(nil), result[index].attention...)
	}
	return result
}

func (inventory *fakeResumeInventory) Discard(
	_ context.Context,
	item resumeStateItem,
) (osfs.DiscardSettlement, error) {
	inventory.record("discard")
	if inventory.closed {
		return osfs.DiscardSettlement{}, errors.New("fake resume inventory used after close")
	}
	inventory.discards = append(inventory.discards, item.number)
	inventory.authorities = append(inventory.authorities, item.authority)
	return inventory.settlement, inventory.discardErr
}

func (inventory *fakeResumeInventory) Close() error {
	inventory.record("close")
	inventory.closeCalls++
	if inventory.closed {
		return errors.New("fake resume inventory closed more than once")
	}
	inventory.closed = true
	return inventory.closeErr
}

func (inventory *fakeResumeInventory) record(event string) {
	inventory.events = append(inventory.events, event)
	if inventory.sharedEvents != nil {
		*inventory.sharedEvents = append(*inventory.sharedEvents, event)
	}
}

type resumeErrorWriter struct{ err error }

func (writer resumeErrorWriter) Write([]byte) (int, error) { return 0, writer.err }

type resumeShortWriter struct{}

func (resumeShortWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}

type resumeErrorReader struct{ err error }

func (reader resumeErrorReader) Read([]byte) (int, error) { return 0, reader.err }

type resumeNthWriteFailure struct {
	writes int
	failAt int
	err    error
	output bytes.Buffer
}

type resumeNthShortWriter struct {
	writes  int
	shortAt int
}

func (writer *resumeNthShortWriter) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.shortAt && len(value) != 0 {
		return len(value) - 1, nil
	}
	return len(value), nil
}

func (writer *resumeNthWriteFailure) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, writer.err
	}
	return writer.output.Write(value)
}

type resumeSequenceWriter struct {
	events *[]string
	labels []string
	writes int
}

func (writer *resumeSequenceWriter) Write(value []byte) (int, error) {
	if writer.writes < len(writer.labels) {
		*writer.events = append(*writer.events, writer.labels[writer.writes])
	}
	writer.writes++
	return len(value), nil
}

type resumeSequenceReader struct {
	events   *[]string
	reader   io.Reader
	recorded bool
}

func (reader *resumeSequenceReader) Read(value []byte) (int, error) {
	if !reader.recorded {
		*reader.events = append(*reader.events, "confirm")
		reader.recorded = true
	}
	return reader.reader.Read(value)
}
