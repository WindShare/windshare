//go:build windows

package outputportable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity(t *testing.T) {
	path := t.TempDir()
	platform, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	owned, err := platform.Root().CreateFile("partial", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	native := owned.(*file)
	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(native.handle.Fd()), &before); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(path, "partial"), filepath.Join(path, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "partial"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementBefore, err := os.Stat(filepath.Join(path, "partial"))
	if err != nil {
		t.Fatal(err)
	}
	// NTFS represents timestamps in 100 ns units.
	modified, err := catalog.NewModifiedTime(1_700_000_000, 123_456_700, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	if matches, err := native.MetadataMatches(4, modified); err != nil || !matches {
		t.Fatalf("native timestamp lost precision: matches=%t err=%v", matches, err)
	}
	var after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(native.handle.Fd()), &after); err != nil {
		t.Fatal(err)
	}
	if after.LastAccessTime != before.LastAccessTime || after.CreationTime != before.CreationTime {
		t.Fatal("mtime update changed unrelated file times")
	}
	replacementAfter, err := os.Stat(filepath.Join(path, "partial"))
	if err != nil || !replacementAfter.ModTime().Equal(replacementBefore.ModTime()) {
		t.Fatalf("mtime update reached replacement file: %v", err)
	}
}
