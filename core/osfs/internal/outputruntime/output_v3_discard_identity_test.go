package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3NestedDiscardCleanupStopsWhenParentIdentityChanges(t *testing.T) {
	for _, replacedParent := range []string{"session", "intent"} {
		t.Run(replacedParent, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			authority := v3RecoveryAuthority(t, root, nil)
			opened := v3RecoveryOpen(t, authority, root, selection)
			session := opened.Session
			sessionID := session.SessionID()
			intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
			sessionName := resumestate.SessionDirectoryName(sessionID)

			levelOne, err := session.stagesDir.CreateDirectory("aa", true)
			if err != nil {
				t.Fatal(err)
			}
			levelTwo, err := levelOne.CreateDirectory("nested", true)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := levelTwo.CreateFile("payload", true, int64(len("payload")))
			if err != nil {
				t.Fatal(err)
			}
			written, err := payload.WriteAt([]byte("payload"), 0)
			if err != nil || written != len("payload") {
				t.Fatalf("write nested payload = (%d, %v)", written, err)
			}
			if err := errors.Join(
				payload.Sync(), levelTwo.Sync(), levelOne.Sync(), session.stagesDir.Sync(),
				payload.Close(), levelTwo.Close(), levelOne.Close(),
			); err != nil {
				t.Fatal(err)
			}

			verifyParents := func() error {
				if err := outputnamespace.VerifyPinnedDirectoryEntry(session.control.Sessions(), intentName, session.intentDir); err != nil {
					return err
				}
				return outputnamespace.VerifyPinnedDirectoryEntry(session.intentDir, sessionName, session.sessionDir)
			}
			replacementCut := 2
			if runtimeDiscardAncestorReplacementMustBeBlocked() {
				// NTFS denies renaming an ancestor while a descendant pin is live.
				// Attempt at the first mutation cut so the denial also proves that
				// cleanup did not remove data before reporting the safety boundary.
				replacementCut = 1
			}
			verifications := 0
			replacementBlocked := false
			var moved, replacement outputcap.Directory
			verifyCut := func() error {
				verifications++
				if verifications != replacementCut {
					return verifyParents()
				}
				var err error
				switch replacedParent {
				case "session":
					moved, err = session.intentDir.InstallDirectoryNoReplace(
						session.sessionDir, "moved-original-session",
					)
					if err == nil {
						replacement, err = session.intentDir.CreateDirectory(sessionName, true)
					}
				case "intent":
					moved, err = session.control.Sessions().InstallDirectoryNoReplace(
						session.intentDir, "moved-original-intent",
					)
					if err == nil {
						replacement, err = session.control.Sessions().CreateDirectory(intentName, true)
					}
				}
				if runtimeDiscardAncestorReplacementMustBeBlocked() {
					if !runtimeDiscardIsBlockedAncestorReplacement(err) {
						t.Fatalf("replace live-pinned %s parent error = %v, want native access denial", replacedParent, err)
					}
					replacementBlocked = true
					return err
				}
				if err != nil {
					t.Fatalf("replace pinned %s parent: %v", replacedParent, err)
				}
				runtimeDiscardWritePrivateSentinel(t, replacement)
				return verifyParents()
			}

			if err := outputnamespace.RemovePrivateDirectoryContents(session.stagesDir, 0, verifyCut); err == nil {
				t.Fatalf("nested discard continued after %s identity replacement", replacedParent)
			}
			if verifications != replacementCut {
				t.Fatalf("authority verifications = %d, want replacement at nested cut %d", verifications, replacementCut)
			}
			if replacementBlocked {
				payloadPath := filepath.Join(
					v3RecoverySessionPath(root, selection, sessionID), resumestate.StagesDirectoryName,
					"aa", "nested", "payload",
				)
				actual, err := os.ReadFile(payloadPath)
				if err != nil || !bytes.Equal(actual, []byte("payload")) {
					t.Fatalf("blocked ancestor replacement allowed cleanup: bytes=%q err=%v", actual, err)
				}
				v3RecoveryCloseSession(t, session)
				return
			}
			actual, err := outputnamespace.ReadRecord(replacement, "sentinel", len("replacement"))
			if err != nil || !bytes.Equal(actual, []byte("replacement")) {
				t.Fatalf("replacement mutated: bytes=%q err=%v", actual, err)
			}
			if err := errors.Join(replacement.Close(), moved.Close()); err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, session)
		})
	}
}

func TestOutputV3DiscardRejectsListedSessionIdentityReplacement(t *testing.T) {
	for _, test := range []struct {
		name      string
		malformed bool
	}{
		{name: "canonical-session"},
		{name: "malformed-session", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			authority := v3RecoveryAuthority(t, root, nil)
			var sessionName string
			if test.malformed {
				sessionName = "malformed-session"
				v3RecoveryCreatePrivateSessionShell(t, authority, root, selection, sessionName)
			} else {
				opened := v3RecoveryOpen(t, authority, root, selection)
				sessionName = resumestate.SessionDirectoryName(opened.Session.SessionID())
				v3RecoveryCloseSession(t, opened.Session)
			}
			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list identity-bound session = %+v", summaries)
			}

			intentPath := filepath.Join(
				root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName,
				resumestate.ResumeNamespaceName(selection.ResumeIntent()),
			)
			sessionPath := filepath.Join(intentPath, sessionName)
			if err := os.Rename(sessionPath, filepath.Join(intentPath, "moved-"+sessionName)); err != nil {
				t.Fatal(err)
			}
			v3RecoveryCreatePrivateSessionShell(t, authority, root, selection, sessionName)

			if _, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference); err == nil ||
				v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
				t.Fatalf("discard replacement error = %v, want session-scoped identity rejection", err)
			}
			actual, err := os.ReadFile(filepath.Join(sessionPath, "sentinel"))
			if err != nil || !bytes.Equal(actual, []byte("replacement")) {
				t.Fatalf("replacement session mutated: bytes=%q err=%v", actual, err)
			}
		})
	}
}

func v3RecoveryCreatePrivateSessionShell(
	t *testing.T,
	authority *Authority,
	root string,
	selection transfer.OutputSelection,
	sessionName string,
) {
	t.Helper()
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	controlResult, err := authority.namespaceController().OpenOrBootstrapControl(platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	control := controlResult.Namespace
	intent, err := outputnamespace.OpenCanonicalIntent(control.Sessions(), selection.ResumeIntent())
	if err != nil {
		t.Fatal(err)
	}
	session, err := intent.CreateDirectory(sessionName, true)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDiscardWritePrivateSentinel(t, session)
	if err := errors.Join(session.Close(), intent.Close(), control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func runtimeDiscardWritePrivateSentinel(t *testing.T, directory outputcap.Directory) {
	t.Helper()
	sentinel, err := directory.CreateFile("sentinel", true, int64(len("replacement")))
	if err != nil {
		t.Fatal(err)
	}
	written, err := sentinel.WriteAt([]byte("replacement"), 0)
	if err != nil || written != len("replacement") {
		t.Fatalf("write replacement sentinel = (%d, %v)", written, err)
	}
	if err := errors.Join(sentinel.Sync(), directory.Sync(), sentinel.Close()); err != nil {
		t.Fatal(err)
	}
}
