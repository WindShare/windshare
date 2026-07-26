package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3ExplicitDiscardRemovesDecodableHeaderWithForeignBinding(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionID := opened.Session.SessionID()
	foreignSelection := v3RecoverySelection(t, true, 1)
	foreignHeader, err := resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID,
		Selection: foreignSelection, OutputRoot: opened.Session.state.Header().OutputRoot(),
		OutputAncestry: v3RecoveryAncestryBinding(
			t, opened.Session.state.Header().OutputRoot(), foreignSelection,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeHeader(foreignHeader)
	if err != nil {
		t.Fatal(err)
	}
	sessionPath := v3RecoverySessionPath(root, selection, sessionID)
	v3RecoveryCloseSession(t, opened.Session)
	headerPath := filepath.Join(sessionPath, resumestate.HeaderRecordName)
	if err := os.WriteFile(headerPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("list foreign-bound header = %+v", summaries)
	}
	if summaries[0].Reference.Kind() != ResumeStateNeedsAttention ||
		!v3RecoveryHasAttention(summaries[0], "session-header-binding") {
		t.Fatalf("foreign-bound header summary = %+v", summaries[0])
	}
	settlement, err := authority.discardResumeState(context.Background(), summaries[0].Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard foreign-bound header = (%+v, %v)", settlement, err)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign-bound session after discard stat error = %v, want not exist", err)
	}
}
