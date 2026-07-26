package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3BoundaryAbsentControlNamespaceClosesSafely(t *testing.T) {
	if err := (*outputControlNamespace)(nil).Close(); err != nil {
		t.Fatalf("close absent control namespace: %v", err)
	}
}

func TestOutputV3BoundaryReopenRetiresSupersededEmptyCandidates(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, &v3RecoverySessionIDs{})

	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, created, err := authority.openOrBootstrapControl(platform)
	if err != nil || !created {
		_ = platform.Close()
		t.Fatalf("bootstrap fixture control = created %t, error %v", created, err)
	}
	installedControl := control.control

	rootCandidateName := v3RecoveryBootstrapCandidateName(t, 0xd1)
	rootCandidate, err := platform.Root().CreateDirectory(rootCandidateName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(rootCandidate.Sync(), platform.Root().Sync(), rootCandidate.Close()); err != nil {
		t.Fatal(err)
	}

	intentName := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	intent, err := openCanonicalIntentDirectory(control.sessions, selection.ResumeIntent())
	if err != nil {
		t.Fatal(err)
	}
	emptySessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0xd2)
	emptyCandidateName := resumestate.SessionCandidateName(emptySessionID)
	emptyCandidate, err := intent.CreateDirectory(emptyCandidateName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(emptyCandidate.Sync(), intent.Sync(), emptyCandidate.Close()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(intent.Close(), control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseSession(t, opened.Session)
	if opened.Session.SessionID() == emptySessionID {
		t.Fatalf("reopen after empty candidate reused discarded session %s", opened.Session.SessionID())
	}
	if opened.Session.control.control != installedControl {
		t.Fatal("candidate cleanup replaced the already-installed global control authority")
	}
	if opened.Session.state.Header().Lifecycle() != resumestate.SessionActive {
		t.Fatalf("reopened lifecycle = %v, want active", opened.Session.state.Header().Lifecycle())
	}

	rootCandidatePath := filepath.Join(root, rootCandidateName)
	if _, err := os.Lstat(rootCandidatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded root bootstrap candidate remained: %v", err)
	}
	emptyCandidatePath := filepath.Join(
		root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName,
		intentName, emptyCandidateName,
	)
	if _, err := os.Lstat(emptyCandidatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session candidate remained after reopen: %v", err)
	}
	installedPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	if info, err := os.Stat(installedPath); err != nil || !info.IsDir() {
		t.Fatalf("replacement session was not installed: info=%v error=%v", info, err)
	}
}
