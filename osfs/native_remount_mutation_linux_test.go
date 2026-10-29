//go:build linux

package osfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func mutateRemountDirectory(t *testing.T, path string) {
	t.Helper()
	var before, after unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &before); err != nil {
		t.Fatal(err)
	}
	// Linux exFAT changes a parent directory's birth time on child rename.
	// Cross its 10 ms timestamp tick so this case cannot pass by clock coincidence.
	const exfatTimestampTick = 10 * time.Millisecond
	time.Sleep(2 * exfatTimestampTick)
	from, to := filepath.Join(path, "ambient-stage"), filepath.Join(path, "ambient-file")
	if err := os.WriteFile(from, []byte("unrelated user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &after); err != nil {
		t.Fatal(err)
	}
	t.Logf("directory=%s birth_time_before=%+v birth_time_after=%+v", path, before.Btime, after.Btime)
}
