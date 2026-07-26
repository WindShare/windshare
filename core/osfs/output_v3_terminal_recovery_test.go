package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RecoversEveryTerminalSessionRemovalCut(t *testing.T) {
	for _, lifecycle := range []resumestate.SessionLifecycle{
		resumestate.SessionCompleting,
		resumestate.SessionDiscarding,
	} {
		for removed := 0; removed <= 5; removed++ {
			t.Run(lifecycle.String()+"/"+outputV3TerminalCutLabel(removed), func(t *testing.T) {
				root := v3RecoveryRoot(t)
				selection := v3RecoverySelection(t, false, 0)
				sessionIDs := &v3RecoverySessionIDs{}
				authority := v3RecoveryAuthority(t, root, sessionIDs)
				opened := v3RecoveryOpen(t, authority, root, selection)
				oldSessionID := opened.Session.SessionID()
				if err := opened.Session.installLifecycle(lifecycle); err != nil {
					t.Fatal(err)
				}
				v3RecoveryCloseSession(t, opened.Session)

				sessionPath := v3RecoverySessionPath(root, selection, oldSessionID)
				removalOrder := []string{
					resumestate.StagesDirectoryName,
					resumestate.AnchorsDirectoryName,
					resumestate.FilesDirectoryName,
					resumestate.SessionLockName,
					resumestate.HeaderRecordName,
				}
				for _, child := range removalOrder[:removed] {
					if err := os.Remove(filepath.Join(sessionPath, child)); err != nil {
						t.Fatalf("remove terminal child %q: %v", child, err)
					}
				}

				reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
				if reopened.Session.SessionID() == oldSessionID {
					t.Fatal("terminal recovery reused the retired session identity")
				}
				if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retired session path stat error = %v, want not exist", err)
				}
				v3RecoveryCloseSession(t, reopened.Session)
			})
		}
	}
}

func TestOutputV3RemovesOnlyTheNamedTerminalSession(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := authority.openOrBootstrapControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intent, err := openCanonicalIntentDirectory(control.sessions, selection.ResumeIntent())
	if err != nil {
		t.Fatal(err)
	}
	targetName := resumestate.SessionDirectoryName(v3RecoveryIdentity16[transfer.OutputSessionID](0x71))
	siblingName := resumestate.SessionDirectoryName(v3RecoveryIdentity16[transfer.OutputSessionID](0x72))
	target, err := intent.CreateDirectory(targetName, true)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := intent.CreateDirectory(siblingName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(target.Sync(), sibling.Sync(), intent.Sync(), sibling.Close()); err != nil {
		t.Fatal(err)
	}

	if err := removeEmptySessionShell(control.sessions, intent, target, intentName, targetName); err != nil {
		t.Fatal(err)
	}
	if kind, err := intent.ObserveEntry(targetName); err != nil || kind != outputV3EntryAbsent {
		t.Fatalf("target session after removal = (%v, %v), want absent", kind, err)
	}
	if kind, err := intent.ObserveEntry(siblingName); err != nil || kind != outputV3EntryDirectory {
		t.Fatalf("sibling session after target removal = (%v, %v), want directory", kind, err)
	}
	if kind, err := control.sessions.ObserveEntry(intentName); err != nil || kind != outputV3EntryDirectory {
		t.Fatalf("non-empty intent after target removal = (%v, %v), want directory", kind, err)
	}
	if err := errors.Join(target.Close(), intent.Close(), control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func outputV3TerminalCutLabel(removed int) string {
	return []string{
		"before-removal",
		"stage-directory-removed",
		"anchor-directory-removed",
		"file-directory-removed",
		"lock-removed",
		"header-removed",
	}[removed]
}
