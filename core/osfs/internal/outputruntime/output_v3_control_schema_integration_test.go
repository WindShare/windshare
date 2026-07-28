package outputruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3InstalledControlSchemaCorruptionBlocksWholeRootWithoutSessionMutation(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	headerPath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.HeaderRecordName,
	)
	headerBefore, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)
	extraPath := filepath.Join(root, resumestate.ControlDirectoryName, "unexpected-control-child")
	if err := os.WriteFile(extraPath, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []transfer.OutputSelection{selection, v3RecoverySelection(t, true, 1)} {
		session, openErr := authority.OpenSelection(context.Background(), candidate)
		if session != nil {
			t.Fatal("corrupt global control returned a session")
		}
		outputV3ControlSessionRequireFault(
			t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		)
	}
	headerAfter, err := os.ReadFile(headerPath)
	if err != nil || !bytes.Equal(headerAfter, headerBefore) {
		t.Fatalf("global schema fault changed session header = %x, %v; want %x", headerAfter, err, headerBefore)
	}
}
