package osfs

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputruntime"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestRecoveryAuthorityUsesClosedNativeErrors(t *testing.T) {
	if !errors.Is(ErrOutputBusy, outputruntime.ErrNativeResumeBusy) ||
		!errors.Is(ErrOutputBusy, resumeauthority.ErrBusy) {
		t.Fatalf("busy error lost its stable semantic identity: %v", ErrOutputBusy)
	}
	if !errors.Is(ErrRecoveryContract, resumeauthority.ErrInvalidContract) {
		t.Fatalf("contract error lost its reducer identity: %v", ErrRecoveryContract)
	}
}

func TestRecoveryAuthorityConstructsForRoot(t *testing.T) {
	authority := NewRecoveryAuthority(t.TempDir())
	if authority == nil {
		t.Fatal("recovery authority is nil")
	}
}
