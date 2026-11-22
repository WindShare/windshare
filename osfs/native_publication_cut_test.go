//go:build windows || linux || darwin

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/output"
)

func pausedPortableFile(t *testing.T, size uint64) (string, transfer.ReceiveIntent) {
	t.Helper()
	return pausedPortableFileAt(t, t.TempDir(), size)
}

func pausedPortableFileAt(t *testing.T, root string, size uint64) (string, transfer.ReceiveIntent) {
	t.Helper()
	ctx := context.Background()
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0x61)
	session, err := operation.OpenDirectTree(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", size, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("file did not start")
	}
	if err := transaction.WriteRange(ctx, 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Pause(ctx, transfer.FilePauseShutdown); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	return root, intent
}

func onlyPortableStage(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".part" {
			if found != "" {
				t.Fatal("multiple staged data objects")
			}
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("missing stage: %s %v", found, err)
	}
	return found
}

func TestPortableDiscardRemovesOnlyPrivateScratchAndPreservesPublicFiles(t *testing.T) {
	ctx := context.Background()
	root, intent := pausedPortableFile(t, 8)
	resultRoot := nativeDirectTreeResultRoot(t, root, intent)
	foreignPath := filepath.Join(resultRoot, "user-owned.txt")
	if err := os.WriteFile(foreignPath, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := onlyPortableStage(t, root)

	recoveryAuthority := NewRecoveryAuthority(root)
	result, err := recoveryAuthority.Discard(ctx, intent.OperationID())
	if err != nil || result.State() != RecoveryOperationDiscarded {
		t.Fatalf("discard = (%+v, %v), want discarded", result, err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged .part survived discard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, output.ControlDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty control directory survived discard: %v", err)
	}
	if actual, err := os.ReadFile(foreignPath); err != nil || string(actual) != "keep-me" {
		t.Fatalf("discard mutated foreign file: %q %v", actual, err)
	}
}
