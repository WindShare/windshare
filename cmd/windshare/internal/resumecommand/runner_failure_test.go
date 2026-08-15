package resumecommand

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunnerFailsClosedAtInjectedPresentationBoundaries(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	t.Run("nil inventory", func(t *testing.T) {
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure || !strings.Contains(stdout.String(), resumeDestinationUnknownReason) {
			t.Fatalf("result=%d stdout=%q", result, stdout.String())
		}
	})
	t.Run("snapshot failure", func(t *testing.T) {
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: &fakeResumeStateInventory{
			snapshot: snapshot, snapshotErr: errors.New("corrupt private record"),
		}}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure || strings.Contains(stdout.String(), "private") {
			t.Fatalf("result=%d stdout=%q", result, stdout.String())
		}
	})
	t.Run("terminal read failure", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{snapshot: snapshot}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{
			interactive: true, err: errors.New("terminal closed"),
		}
		if result := app.Run(context.Background(), []string{
			"resume", "discard", "-o", t.TempDir(), "--item", "1",
		}); result != ResultFailure || inventory.discardCalls != 0 ||
			!strings.Contains(stderr.String(), "could not be read") {
			t.Fatalf("result=%d inventory=%+v stderr=%q", result, inventory, stderr.String())
		}
	})
	t.Run("result write failure", func(t *testing.T) {
		app, _, _ := newResumeTestApp()
		app.stdout = errorResumeWriter{}
		app.resumeInventories = &fakeResumeStateInventoryOpener{
			inventory: &fakeResumeStateInventory{snapshot: snapshot},
		}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
	})
}

type errorResumeWriter struct{}

func (errorResumeWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected output failure")
}
