package osfs

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

func TestFilesystemResumeStateAuthorityUsesClosedNativeErrors(t *testing.T) {
	if !errors.Is(ErrResumeStateBusy, outputruntime.ErrNativeResumeBusy) ||
		!errors.Is(ErrResumeStateBusy, resumeauthority.ErrBusy) {
		t.Fatalf("busy error lost its stable semantic identity: %v", ErrResumeStateBusy)
	}
	if !errors.Is(ErrResumeStateContract, resumeauthority.ErrInvalidContract) {
		t.Fatalf("contract error lost its reducer identity: %v", ErrResumeStateContract)
	}
}

func TestFilesystemResumeStateAuthorityImplementsPublicContract(t *testing.T) {
	authority, err := NewFilesystemResumeStateAuthority(
		FilesystemResumeRoot{RootPath: t.TempDir()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if authority == nil {
		t.Fatal("filesystem resume authority is nil")
	}
	var _ ResumeStateAuthority = authority
}
