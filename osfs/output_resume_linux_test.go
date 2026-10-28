//go:build linux

package osfs

import (
	"context"
	"testing"
)

func TestLinuxTmpfsDefaultOutputRetainsRangesAcrossReopen(t *testing.T) {
	t.Parallel()
	root := linuxPublicTmpfsRoot(t)
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	mode, bindErr := authority.BindDestination(context.Background())
	closeErr := authority.Close()
	if bindErr != nil || closeErr != nil || !mode.Resumable() {
		t.Fatalf("tmpfs lost portable recovery: mode=%+v bind=%v close=%v", mode, bindErr, closeErr)
	}
	runPortableResumeAcceptsMtimeDriftAndFlushesPause(t, root)
}
