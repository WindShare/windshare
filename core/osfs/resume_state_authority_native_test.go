//go:build windows || linux

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/transfer"
)

func TestFilesystemResumeStateAuthorityListsAbsentStateWithoutCreatingNamespace(t *testing.T) {
	rootFixture := testoutputroot.New(t)
	if err := os.Mkdir(rootFixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{
		RootPath: rootFixture.RootPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("absent-state summaries = %+v", summaries)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(filepath.Join(rootFixture.RootPath, ".windshare-output"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only list created control namespace: %v", err)
	}
}

func TestFilesystemResumeStateAuthorityRejectsRelativeRoot(t *testing.T) {
	if _, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: "relative"}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative-root constructor error = %v", err)
	}
}
