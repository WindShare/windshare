package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RecoversInterruptedHeaderReplacementBeforeExactChildValidation(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	updated, err := opened.Session.state.WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeHeader(updated.Header())
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x72}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	// Match the public state-store naming contract so reopening treats this as
	// an interrupted header replacement rather than an arbitrary private file.
	temporaryName, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := opened.Session.sessionDir.CreateFile(temporaryName, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	written, err := temporary.WriteAt(encoded, 0)
	if err != nil || written != len(encoded) {
		t.Fatalf("write header temporary = (%d, %v), want %d", written, err, len(encoded))
	}
	if err := errors.Join(temporary.Sync(), opened.Session.sessionDir.Sync(), temporary.Close()); err != nil {
		t.Fatal(err)
	}
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	v3RecoveryCloseSession(t, opened.Session)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	if reopened.Session.state.Header().Lifecycle() != resumestate.SessionActive {
		t.Fatalf("authoritative lifecycle = %v, want active", reopened.Session.state.Header().Lifecycle())
	}
	if _, err := os.Stat(filepath.Join(sessionPath, temporaryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("header update temporary stat error = %v, want not exist", err)
	}
	v3RecoveryCloseSession(t, reopened.Session)
}

func TestOutputV3CorruptHeaderIsIntentScopedAndExplicitlyDiscardable(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	v3RecoveryCloseSession(t, opened.Session)
	if err := os.WriteFile(filepath.Join(sessionPath, resumestate.HeaderRecordName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Reference.Kind() != ResumeStateNeedsAttention {
		t.Fatalf("corrupt-header summaries = %+v, want one needs-attention reference", summaries)
	}
	if !runtimeCorruptionHasAttention(summaries[0], "session-header-corrupt") {
		t.Fatalf("corrupt-header attention = %+v", summaries[0].Attention)
	}
	if _, err := authority.OpenSelection(context.Background(), selection); err == nil || v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
		t.Fatalf("open corrupt intent error = %v, want session-scoped fault", err)
	}

	// An unrelated canonical selection must remain usable even while the damaged
	// intent is awaiting an explicit discard decision.
	other := v3RecoverySelection(t, true, 1)
	otherOpen := v3RecoveryOpen(t, authority, root, other)
	v3RecoveryCloseSession(t, otherOpen.Session)

	settlement, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Kind != Discarded {
		t.Fatalf("discard kind = %v, want %v", settlement.Kind, Discarded)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded corrupt session stat error = %v, want not exist", err)
	}
}

func TestOutputV3CorruptControlBlocksTheRootAndCannotBeDiscardedThroughSessionRef(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	v3RecoveryCloseSession(t, opened.Session)

	beforeInventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, beforeInventory)
	before := beforeInventory.Summaries()
	if len(before) != 1 {
		t.Fatalf("list before control corruption = %+v", before)
	}
	controlPath := filepath.Join(root, resumestate.ControlDirectoryName, resumestate.ControlRecordName)
	if err := os.WriteFile(controlPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	failedInventory, err := authority.ListResumeState(context.Background(), root)
	if failedInventory != nil {
		defer v3RecoveryCloseInventory(t, failedInventory)
	}
	if err == nil || v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
		t.Fatalf("list corrupt control error = %v, want root-scoped fault", err)
	}
	if _, err := authority.DiscardResumeState(context.Background(), before[0].Reference); err == nil ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultRoot {
		t.Fatalf("discard under corrupt control error = %v, want root-scoped fault", err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session changed despite corrupt global control: %v", err)
	}
}

func TestOutputV3ExplicitDiscardRemovesUnknownPrivateEntriesWithoutFollowingLinks(t *testing.T) {
	t.Parallel()
	for _, entryKind := range []string{"regular-file", "symbolic-link"} {
		t.Run(entryKind, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			authority := v3RecoveryAuthority(t, root, nil)
			opened := v3RecoveryOpen(t, authority, root, selection)
			sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
			unknownPath := filepath.Join(sessionPath, "unknown-private-entry")
			var externalPath string
			switch entryKind {
			case "regular-file":
				unknown, err := opened.Session.sessionDir.CreateFile("unknown-private-entry", true, int64(len("unknown")))
				if err != nil {
					t.Fatal(err)
				}
				written, err := unknown.WriteAt([]byte("unknown"), 0)
				if err != nil || written != len("unknown") {
					t.Fatalf("write unknown private entry = (%d, %v)", written, err)
				}
				if err := errors.Join(unknown.Sync(), opened.Session.sessionDir.Sync(), unknown.Close()); err != nil {
					t.Fatal(err)
				}
			case "symbolic-link":
				externalPath = filepath.Join(t.TempDir(), "outside-sentinel")
				if err := os.WriteFile(externalPath, bytes.Repeat([]byte{0x5c}, 8), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(externalPath, unknownPath); err != nil {
					v3RecoveryCloseSession(t, opened.Session)
					t.Skipf("symbolic-link creation unavailable: %v", err)
				}
			}
			v3RecoveryCloseSession(t, opened.Session)

			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list unknown private entry = %+v", summaries)
			}
			settlement, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference)
			if err != nil {
				t.Fatal(err)
			}
			if settlement.Kind != Discarded {
				t.Fatalf("discard kind = %v, want %v", settlement.Kind, Discarded)
			}
			if _, err := os.Lstat(sessionPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("discarded unsafe session lstat error = %v, want not exist", err)
			}
			if externalPath != "" {
				actual, err := os.ReadFile(externalPath)
				if err != nil || !bytes.Equal(actual, bytes.Repeat([]byte{0x5c}, 8)) {
					t.Fatalf("discard followed private link: bytes=%x err=%v", actual, err)
				}
			}
		})
	}
}

func runtimeCorruptionHasAttention(summary ResumeStateSummary, code string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == code {
			return true
		}
	}
	return false
}
