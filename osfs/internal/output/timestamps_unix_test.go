//go:build linux || darwin

package output

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

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
			root, _, err := OpenContainer(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			owned, err := openChildFile(root, "partial", os.O_RDWR|os.O_CREATE|os.O_EXCL, privateFileMode)
			if err != nil {
				t.Fatal(err)
			}
			defer owned.Close()
			atime := time.Unix(1600000000, 987654321)
			initial := time.Unix(1600000010, 123456789)
			originalPath := filepath.Join(path, "partial")
			if err := os.Chtimes(originalPath, atime, initial); err != nil {
				t.Fatal(err)
			}
			var before unix.Stat_t
			if err := unix.Fstat(int(owned.Fd()), &before); err != nil {
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
			if err := setFileModifiedTimeCatalog(owned, modified); err != nil {
				t.Fatal(err)
			}
			var after unix.Stat_t
			if err := unix.Fstat(int(owned.Fd()), &after); err != nil {
				t.Fatal(err)
			}
			if int64(after.Mtim.Sec) != modified.Seconds() || int64(after.Mtim.Nsec) != int64(modified.Nanoseconds()) {
				t.Fatalf("mtime lost precision: %+v", after.Mtim)
			}
			if after.Atim != before.Atim {
				t.Fatalf("mtime update changed atime: %+v -> %+v", before.Atim, after.Atim)
			}
			if matches, err := fileMetadataMatches(owned, 0, modified); err != nil || !matches {
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

func TestSetDirModifiedTimeStaysAnchoredPreservesAccessTimeAndAvoidsUnixNanoWrap(t *testing.T) {
	base := t.TempDir()
	origPath := filepath.Join(base, "target")
	movedPath := filepath.Join(base, "moved")
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	atime := time.Unix(1_600_000_000, 987_654_321)
	initial := time.Unix(1_600_000_010, 123_456_789)
	if err := os.Chtimes(origPath, atime, initial); err != nil {
		t.Fatal(err)
	}
	dir, _, err := OpenContainer(origPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	var before unix.Stat_t
	if err := unix.Fstat(int(dir.handle.Fd()), &before); err != nil {
		t.Fatal(err)
	}

	// Move the open directory and place a decoy directory at its original path.
	if err := os.Rename(origPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Stat(origPath)
	if err != nil {
		t.Fatal(err)
	}

	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_456_789, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := setDirModifiedTimeCatalog(dir, modified); err != nil {
		t.Fatalf("setDirModifiedTimeCatalog: %v", err)
	}

	var after unix.Stat_t
	if err := unix.Fstat(int(dir.handle.Fd()), &after); err != nil {
		t.Fatal(err)
	}
	if int64(after.Mtim.Sec) != modified.Seconds() || int64(after.Mtim.Nsec) != int64(modified.Nanoseconds()) {
		t.Fatalf("directory mtime = %+v, want sec=%d nsec=%d", after.Mtim, modified.Seconds(), modified.Nanoseconds())
	}
	if after.Atim != before.Atim {
		t.Fatalf("directory mtime update changed atime: %+v -> %+v", before.Atim, after.Atim)
	}
	replacementAfter, err := os.Stat(origPath)
	if err != nil || !replacementAfter.ModTime().Equal(replacementBefore.ModTime()) {
		t.Fatalf("directory mtime update reached replacement: before=%v after=%v err=%v", replacementBefore.ModTime(), replacementAfter.ModTime(), err)
	}

	// Year 2300 exceeds time.Time.UnixNano (2262) while fitting ext4 (2446) and
	// APFS (2554). Ensure 64-bit timespec targets never silently wrap to ~1715.
	if unsafe.Sizeof(unix.Timespec{}.Sec) < 8 {
		return
	}
	futureSec := time.Date(2300, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	future, err := catalog.NewModifiedTime(futureSec, 123_456_789, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := setDirModifiedTimeCatalog(dir, future); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ERANGE) || errors.Is(err, unix.EOVERFLOW) {
			return
		}
		t.Fatalf("setDirModifiedTimeCatalog(2300): %v", err)
	}
	if err := unix.Fstat(int(dir.handle.Fd()), &after); err != nil {
		t.Fatal(err)
	}
	if int64(after.Mtim.Sec) != futureSec || int64(after.Mtim.Nsec) != 123_456_789 {
		t.Fatalf("future directory mtime = %+v, want sec=%d nsec=123456789", after.Mtim, futureSec)
	}
}

func TestDirectoryEntryMatchesSpellingStaysAnchoredAfterParentMoved(t *testing.T) {
	base := t.TempDir()
	origPath := filepath.Join(base, "parent")
	movedPath := filepath.Join(base, "moved-parent")
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "item.txt"), []byte("orig"), 0o600); err != nil {
		t.Fatal(err)
	}

	parent, _, err := OpenContainer(origPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	var observed unix.Stat_t
	if err := unix.Fstatat(int(parent.handle.Fd()), "item.txt", &observed, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}

	// Move parent and install a decoy directory containing a different inode at
	// the stale parent.path so path-based os.Lstat would fail or mismatch.
	if err := os.Rename(origPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(origPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origPath, "item.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	var decoyStat unix.Stat_t
	if err := unix.Lstat(filepath.Join(origPath, "item.txt"), &decoyStat); err != nil {
		t.Fatal(err)
	}

	exact, err := directoryEntryMatchesSpelling(parent, "item.txt", observed)
	if err != nil || !exact {
		t.Fatalf("directoryEntryMatchesSpelling exact after move = (%t, %v), want (true, nil)", exact, err)
	}
	exact, err = directoryEntryMatchesSpelling(parent, "ITEM.TXT", observed)
	if err != nil || exact {
		t.Fatalf("directoryEntryMatchesSpelling wrong case = (%t, %v), want (false, nil)", exact, err)
	}
	exact, err = directoryEntryMatchesSpelling(parent, "item.txt", decoyStat)
	if exact || !errors.Is(err, ErrUnsafeNamespace) {
		t.Fatalf("directoryEntryMatchesSpelling mismatched inode = (%t, %v), want (false, ErrUnsafeNamespace)", exact, err)
	}
}
