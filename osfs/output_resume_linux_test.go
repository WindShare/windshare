//go:build linux

package osfs

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxTmpfsDefaultOutputRetainsRangesAcrossReopen(t *testing.T) {
	t.Parallel()
	root := linuxPublicTmpfsRoot(t)
	authority, err := NewOutputAuthority(OutputConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	bindErr := authority.BindDestination(context.Background())
	closeErr := authority.Close()
	if bindErr != nil || closeErr != nil {
		t.Fatalf("tmpfs lost portable recovery: bind=%v close=%v", bindErr, closeErr)
	}
	runPortableResumeAcceptsMtimeDriftAndFlushesPause(t, root)
}

func linuxPublicTmpfsRoot(t *testing.T) string {
	t.Helper()
	const tmpfsRoot = "/dev/shm"
	var filesystem unix.Statfs_t
	if err := unix.Statfs(tmpfsRoot, &filesystem); err != nil || filesystem.Type != unix.TMPFS_MAGIC {
		t.Skipf("native tmpfs fixture unavailable: %v", err)
	}
	root, err := os.MkdirTemp(tmpfsRoot, "windshare-public-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	return root
}
