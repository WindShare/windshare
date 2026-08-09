package resumecommand

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
)

func TestRunnerFailsClosedAtInjectedCapabilityBoundaries(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	t.Run("missing action", func(t *testing.T) {
		app, _, stderr := newResumeTestApp()
		if result := app.Run(ctx, []string{"resume"}); result != ResultUsage {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), "exactly one action") ||
			!strings.Contains(stderr.String(), "windshare resume list") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("list nil inventory", func(t *testing.T) {
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{}
		if result := app.Run(ctx, []string{"resume", "list", "-o", root}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), errResumeStateContract.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("list item projection failure", func(t *testing.T) {
		itemErr := errors.New("project inventory failed")
		inventory := &fakeResumeStateInventory{itemsErr: itemErr}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		if result := app.Run(ctx, []string{"resume", "list", "-o", root}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), itemErr.Error()) {
			t.Fatalf("inventory=%+v stderr=%q", inventory, stderr.String())
		}
	})

	t.Run("list invalid item", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{{}}}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		if result := app.Run(ctx, []string{"resume", "list", "-o", root}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), "render live inventory") {
			t.Fatalf("inventory=%+v stderr=%q", inventory, stderr.String())
		}
	})

	t.Run("discard nil inventory", func(t *testing.T) {
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), errResumeStateContract.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("discard item projection failure", func(t *testing.T) {
		itemErr := errors.New("project selected item failed")
		inventory := &fakeResumeStateInventory{itemsErr: itemErr}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), itemErr.Error()) {
			t.Fatalf("inventory=%+v stderr=%q", inventory, stderr.String())
		}
	})

	t.Run("discard invalid item", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{{}}}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), "selected item") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("confirmation read failure", func(t *testing.T) {
		readErr := errors.New("terminal read failed")
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{availableResumeItem()}}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, err: readErr}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if inventory.discardCalls != 0 || !strings.Contains(stderr.String(), readErr.Error()) {
			t.Fatalf("inventory=%+v stderr=%q", inventory, stderr.String())
		}
	})

	t.Run("discard dependency failure", func(t *testing.T) {
		discardErr := errors.New("discard failed")
		inventory := &fakeResumeStateInventory{
			items:      []resumeStateItem{availableResumeItem()},
			discardErr: discardErr,
		}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), discardErr.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("invalid discard settlement", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{items: []resumeStateItem{availableResumeItem()}}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), "invalid settlement") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("discard output failure", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{
			items:         []resumeStateItem{availableResumeItem()},
			discardReport: settledResumeDiscardReport(),
		}
		app, _, stderr := newResumeTestApp()
		app.Stdout = shortResumeWriter{}
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{interactive: true, line: "discard 1"}
		if result := app.Run(ctx, []string{
			"resume", "discard", "-o", root, "--item", "1",
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), io.ErrShortWrite.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("legacy dependency failure", func(t *testing.T) {
		cleanupErr := errors.New("legacy cleanup failed")
		app, _, stderr := newResumeTestApp()
		app.legacyResumeCleaner = &fakeLegacyResumeCleaner{err: cleanupErr}
		if result := app.Run(ctx, []string{"resume", "cleanup", "-o", root}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), cleanupErr.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})

	t.Run("legacy output failure", func(t *testing.T) {
		app, _, stderr := newResumeTestApp()
		app.Stdout = shortResumeWriter{}
		app.legacyResumeCleaner = &fakeLegacyResumeCleaner{report: osfs.CheckpointCleanupReport{
			Status:   osfs.CheckpointCleanupStatusComplete,
			Complete: true,
		}}
		if result := app.Run(ctx, []string{"resume", "cleanup", "-o", root}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
		if !strings.Contains(stderr.String(), io.ErrShortWrite.Error()) {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}
