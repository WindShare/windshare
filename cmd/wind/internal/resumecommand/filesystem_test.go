package resumecommand

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/output"
)

func TestFilesystemRunnerHelpUsesOnlyRootOwnedOperationInventory(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runner := NewFilesystemRunner(FilesystemConfig{
		Input: strings.NewReader(""), Output: stdout,
		RawTerminalOutput: stderr, DiagnosticOutput: testDiagnosticOutput{writer: stderr},
	})
	if result := runner.Run(context.Background(), []string{"help"}); result != command.ExitOK {
		t.Fatalf("result=%d", result)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "resume list -o") ||
		!strings.Contains(stderr.String(), "resume discard -o") ||
		strings.Contains(stderr.String(), "resume cleanup") || strings.Contains(stderr.String(), "legacy") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
func TestFilesystemRunnerUsesEngineRecoveryWithoutCreatingMissingRoot(t *testing.T) {
	application, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	missing := filepath.Join(t.TempDir(), "missing")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	runner := NewFilesystemRunner(FilesystemConfig{
		Recovery: application, Input: strings.NewReader(""), Output: stdout,
		RawTerminalOutput: stderr, DiagnosticOutput: testDiagnosticOutput{writer: stderr},
		Logf: func(format string, args ...any) { _, _ = fmt.Fprintf(stderr, format, args...) },
	})
	if result := runner.Run(context.Background(), []string{"list", "-o", missing}); result != command.ExitFailure {
		t.Fatalf("missing destination result=%d", result)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resume created destination: %v", err)
	}
	if !strings.Contains(stdout.String(), resumeDestinationBindingReason) || stderr.Len() == 0 {
		t.Fatalf("engine failure projection stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestResumeDiscardProjectsEngineRefusalAfterConfirmation(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot([]resumeOperation{testResumeOperation("1", resumeOperationResumable)}, false)
	for _, test := range []struct {
		name    string
		failure *engine.RecoveryFailure
		status  string
	}{
		{"changed", &engine.RecoveryFailure{Kind: engine.RecoveryFailureChanged, Reason: resumeOperationChangedReason}, resumeDiscardStatusChanged},
		{"busy", &engine.RecoveryFailure{Kind: engine.RecoveryFailureBusy, Reason: resumeOperationRunningReason}, resumeBusyStatus},
		{"cancelled", &engine.RecoveryFailure{Kind: engine.RecoveryFailureCancelled, Reason: resumeCommandCancelledReason}, resumeCancelledStatus},
		{"attention", &engine.RecoveryFailure{Kind: engine.RecoveryFailureNeedsAttention, Reason: resumeOperationUnknownReason}, resumeDiscardStatusNeedsAttention},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := &fakeResumeStateInventory{snapshot: snapshot, discardFailure: test.failure}
			terminal := &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
			app, stdout, _ := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
			app.resumeConfirmation = terminal
			result := app.Run(context.Background(), []string{"resume", "discard", "-o", t.TempDir(), "--item", "1"})
			if result != command.ExitFailure || terminal.calls != 1 || inventory.discardCalls != 1 ||
				inventory.discardID != snapshot.Operations[0].ID {
				t.Fatalf("identity-bound interaction result=%d inventory=%+v terminal=%+v", result, inventory, terminal)
			}
			if !strings.Contains(stdout.String(), fmt.Sprintf("resume_discard_status=%q", test.status)) ||
				!strings.Contains(stdout.String(), fmt.Sprintf("reason=%q", test.failure.Reason)) {
				t.Fatalf("engine decision was lost: %q", stdout.String())
			}
		})
	}
}

func TestFilesystemResumeDiscardRaceOmitsMalformedStageAndNativeClass(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		removeDir func(root, opDir string) string
	}{
		{
			name:      "operation directory disappears during confirmation",
			removeDir: func(_, opDir string) string { return opDir },
		},
		{
			name:      "control directory disappears during confirmation",
			removeDir: func(root, _ string) string { return filepath.Join(root, ".windshare-tmp") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			opID := createRetainedFilesystemOp(t, ctx, root, 0x21)
			opDir := filepath.Join(root, ".windshare-tmp", hex.EncodeToString(opID.Bytes()))

			application, err := engine.New(engine.Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := application.Close(context.Background()); closeErr != nil {
					t.Error(closeErr)
				}
			})

			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			logger := logFunc(func(format string, values ...any) {
				_, _ = fmt.Fprintf(stderr, format+"\n", values...)
			})
			runner := newRunner(resumeDependencies{
				inventories: filesystemResumeStateInventoryOpener{recovery: application},
				confirmation: callbackConfirmationTerminal{
					onReadLine: func() (string, error) {
						if err := os.RemoveAll(tc.removeDir(root, opDir)); err != nil {
							return "", err
						}
						return "discard 1", nil
					},
				},
				parser:   flagRequestParser{},
				renderer: textRenderer{},
				output:   streamResumeOutput{result: stdout, usage: testDiagnosticOutput{writer: stderr}},
				logger:   logger,
			})

			result := runner.Run(ctx, []string{"discard", "-o", root, "--item", "1"})
			rendered := stdout.String()
			if result != command.ExitFailure ||
				!strings.Contains(rendered, `resume_discard_status="operation-changed"`) ||
				!strings.Contains(rendered, `reason="operation-no-longer-matches"`) {
				t.Fatalf("result=%d stdout=%q stderr=%q", result, rendered, stderr.String())
			}
			if strings.Contains(rendered, "stage=") || strings.Contains(rendered, "native_error_class=") {
				t.Fatalf("operation-changed leaked diagnostic detail: stdout=%q", rendered)
			}
		})
	}
}

type callbackConfirmationTerminal struct {
	onReadLine func() (string, error)
}

func (callbackConfirmationTerminal) Interactive() bool { return true }

func (terminal callbackConfirmationTerminal) ReadLine(context.Context, string) (string, error) {
	return terminal.onReadLine()
}

func createRetainedFilesystemOp(
	t *testing.T,
	ctx context.Context,
	root string,
	seed byte,
) receivecontract.OperationID {
	t.Helper()
	authority, err := filesystem.FilesystemOutput{RootPath: root, CreateRoot: true}.NewOutputAuthority(output.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := authority.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
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
	selection, err := transfer.NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind != output.OutputLookupCreateReady || lookup.Reservation == nil {
		t.Fatalf("SelectOperation = (%+v, %v)", lookup, err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Reservation.Create(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Intent.OperationID()
}
