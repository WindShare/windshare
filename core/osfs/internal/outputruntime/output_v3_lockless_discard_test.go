package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ExplicitDiscardRecognizesLocklessFixedSessionCuts(t *testing.T) {
	t.Run("exact empty terminal shell", func(t *testing.T) {
		root, authority, selection, sessionPath := v3LocklessDiscardFixture(t)
		for _, name := range []string{
			resumestate.StagesDirectoryName,
			resumestate.AnchorsDirectoryName,
			resumestate.FilesDirectoryName,
			resumestate.SessionLockName,
			resumestate.HeaderRecordName,
		} {
			if err := os.Remove(filepath.Join(sessionPath, name)); err != nil {
				t.Fatalf("remove %s from exact empty cut: %v", name, err)
			}
		}

		inventory, summary := v3ListSingleLocklessSession(t, authority, root)
		defer v3RecoveryCloseInventory(t, inventory)
		if summary.Reference.ResumeIntent() != selection.ResumeIntent() ||
			summary.Reference.Kind() != ResumeStateNeedsAttention {
			t.Fatalf("exact-empty summary = %+v", summary)
		}
		settlement, err := authority.DiscardResumeState(context.Background(), summary.Reference)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("discard exact empty terminal shell = (%+v, %v)", settlement, err)
		}
		if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("exact empty shell after discard stat error = %v", err)
		}
	})

	t.Run("corrupt header after lock retirement", func(t *testing.T) {
		root, authority, _, sessionPath := v3LocklessDiscardFixture(t)
		if err := os.Remove(filepath.Join(sessionPath, resumestate.SessionLockName)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(sessionPath, resumestate.HeaderRecordName), []byte("corrupt"), 0o600,
		); err != nil {
			t.Fatal(err)
		}

		inventory, summary := v3ListSingleLocklessSession(t, authority, root)
		defer v3RecoveryCloseInventory(t, inventory)
		if !runtimeLocklessDiscardHasAttention(summary, "session-header-corrupt") {
			t.Fatalf("corrupt lockless summary = %+v", summary)
		}
		settlement, err := authority.DiscardResumeState(context.Background(), summary.Reference)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("discard fixed corrupt lockless session = (%+v, %v)", settlement, err)
		}
		if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("corrupt lockless session after discard stat error = %v", err)
		}
	})

	t.Run("valid active header without lock is not a terminal cut", func(t *testing.T) {
		root, authority, _, sessionPath := v3LocklessDiscardFixture(t)
		if err := os.Remove(filepath.Join(sessionPath, resumestate.SessionLockName)); err != nil {
			t.Fatal(err)
		}

		inventory, summary := v3ListSingleLocklessSession(t, authority, root)
		defer v3RecoveryCloseInventory(t, inventory)
		if !runtimeLocklessDiscardHasAttention(summary, "session-lock-missing") {
			t.Fatalf("active lockless summary = %+v", summary)
		}
		if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
			!errors.Is(err, outputfault.ErrIntentUnsafe) || v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
			t.Fatalf("discard active lockless session error = %v, want session-scoped unsafe cut", err)
		}
		if _, err := os.Stat(sessionPath); err != nil {
			t.Fatalf("rejected active lockless discard changed session: %v", err)
		}
	})
}

func v3LocklessDiscardFixture(
	t *testing.T,
) (string, *Authority, transfer.OutputSelection, string) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	v3RecoveryCloseSession(t, opened.Session)
	return root, authority, selection, sessionPath
}

func v3ListSingleLocklessSession(
	t *testing.T,
	authority *Authority,
	root string,
) (*ResumeStateInventory, ResumeStateSummary) {
	t.Helper()
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		_ = inventory.Close()
		t.Fatalf("lockless resume summaries = %+v, want one", summaries)
	}
	return inventory, summaries[0]
}

func runtimeLocklessDiscardHasAttention(summary ResumeStateSummary, expected string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == expected {
			return true
		}
	}
	return false
}
