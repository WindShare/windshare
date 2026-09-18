package resumecommand

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine"
)

func TestFilesystemRunnerHelpUsesOnlyRootOwnedOperationInventory(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runner := NewFilesystemRunner(FilesystemConfig{
		Input: strings.NewReader(""), Output: stdout,
		RawTerminalOutput: stderr, SerializedTerminalOutput: stderr,
	})
	if result := runner.Run(context.Background(), []string{"help"}); result != ResultOK {
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
		RawTerminalOutput: stderr, SerializedTerminalOutput: stderr,
		Logf: func(format string, args ...any) { _, _ = fmt.Fprintf(stderr, format, args...) },
	})
	if result := runner.Run(context.Background(), []string{"list", "-o", missing}); result != ResultFailure {
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
			if result != ResultFailure || terminal.calls != 1 || inventory.discardCalls != 1 ||
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
