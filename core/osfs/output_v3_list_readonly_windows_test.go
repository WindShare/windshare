//go:build windows

package osfs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"
)

type windowsV3ObjectIDMutationTrap struct {
	calls atomic.Int64
}

func (trap *windowsV3ObjectIDMutationTrap) CreateOrGet(
	windows.Handle,
) (windowsV3PersistentObjectID, error) {
	trap.calls.Add(1)
	return windowsV3PersistentObjectID{}, errors.New("unexpected Object ID mutation")
}

func trapWindowsV3ObjectIDMutation(authority *FilesystemOutputAuthority) *windowsV3ObjectIDMutationTrap {
	trap := &windowsV3ObjectIDMutationTrap{}
	original := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := original(path, create)
		if err != nil {
			return nil, err
		}
		windowsPlatform := platform.(*windowsOutputV3Platform)
		windowsPlatform.native.root.objectIDs = trap
		return platform, nil
	}
	return trap
}

func TestOutputV3ListCleanNTFSRootDoesNotCreatePersistentIdentity(t *testing.T) {
	rootPath := t.TempDir()
	before, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Skipf("certified NTFS output root unavailable: %v", err)
	}
	if names, err := before.Root().Names(1); err != nil || len(names) != 0 {
		_ = before.Close()
		t.Fatalf("clean NTFS root entries before list = %v, %v", names, err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	trap := trapWindowsV3ObjectIDMutation(authority)
	inventory, err := authority.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: rootPath},
	)
	if err != nil {
		t.Fatalf("list clean NTFS root: %v", err)
	}
	if summaries := inventory.Summaries(); len(summaries) != 0 {
		t.Fatalf("clean NTFS root resume summaries = %+v", summaries)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if calls := trap.calls.Load(); calls != 0 {
		t.Fatalf("read-only resume inventory invoked CreateOrGet %d times", calls)
	}
	if names, err := after.Root().Names(1); err != nil || len(names) != 0 {
		t.Fatalf("clean NTFS root entries after list = %v, %v", names, err)
	}
}
