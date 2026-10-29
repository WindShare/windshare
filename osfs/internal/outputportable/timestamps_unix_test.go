//go:build linux || darwin

package outputportable

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/unix"
)

func TestSetModifiedTimePreservesNanosecondsAccessTimeAndHandleIdentity(t *testing.T) {
	for _, replaceName := range []bool{false, true} {
		name := "original name"
		if replaceName {
			name = "replaced name"
		}
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			platform, err := Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			owned, err := platform.Root().CreateFile("partial", true, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer owned.Close()
			native := owned.(*file)
			atime := time.Unix(1600000000, 987654321)
			initial := time.Unix(1600000010, 123456789)
			originalPath := filepath.Join(path, "partial")
			if err := os.Chtimes(originalPath, atime, initial); err != nil {
				t.Fatal(err)
			}
			var before unix.Stat_t
			if err := unix.Fstat(int(native.handle.Fd()), &before); err != nil {
				t.Fatal(err)
			}
			var replacementBefore os.FileInfo
			if replaceName {
				if err := os.Rename(originalPath, filepath.Join(path, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(originalPath, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
				replacementBefore, err = os.Stat(originalPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			modified, err := catalog.NewModifiedTime(1700000000, 123456789, catalog.TimePrecisionNanoseconds)
			if err != nil {
				t.Fatal(err)
			}
			if err := owned.SetModifiedTime(modified); err != nil {
				t.Fatal(err)
			}
			var after unix.Stat_t
			if err := unix.Fstat(int(native.handle.Fd()), &after); err != nil {
				t.Fatal(err)
			}
			if int64(after.Mtim.Sec) != modified.Seconds() || int64(after.Mtim.Nsec) != int64(modified.Nanoseconds()) {
				t.Fatalf("mtime lost precision: %+v", after.Mtim)
			}
			if after.Atim != before.Atim {
				t.Fatalf("mtime update changed atime: %+v -> %+v", before.Atim, after.Atim)
			}
			if matches, err := native.MetadataMatches(0, modified); err != nil || !matches {
				t.Fatalf("written metadata does not match source: %v, %v", matches, err)
			}
			if replaceName {
				replacementAfter, err := os.Stat(originalPath)
				if err != nil || !replacementAfter.ModTime().Equal(replacementBefore.ModTime()) {
					t.Fatalf("timestamp update reached the replacement: %v", err)
				}
			}
		})
	}
}
