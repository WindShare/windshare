//go:build darwin

package nativeoutputdarwin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGoNativeOutputPrimitives(t *testing.T) {
	path := t.TempDir()
	root, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	const mode = 0o600
	stage, err := unix.Openat(root, "stage", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW, mode)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(stage)
	data := []byte("retained verified range")
	if n, err := unix.Pwrite(stage, data, 0); err != nil || n != len(data) {
		t.Fatalf("range write = %d, %v", n, err)
	}
	if err := unix.Fsync(stage); err != nil {
		t.Fatal(err)
	}
	var before unix.Stat_t
	if err := unix.Fstat(stage, &before); err != nil {
		t.Fatal(err)
	}
	if err := unix.Linkat(root, "stage", root, "anchor", 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "final"), []byte("foreign"), mode); err != nil {
		t.Fatal(err)
	}
	flags := uint32(unix.RENAME_EXCL | unix.RENAME_NOFOLLOW_ANY)
	if err := unix.RenameatxNp(root, "stage", root, "final", flags); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("publication collision = %v", err)
	}
	foreign, err := os.ReadFile(filepath.Join(path, "final"))
	if err != nil || string(foreign) != "foreign" {
		t.Fatalf("collision changed existing output: %q, %v", foreign, err)
	}
	if err := unix.Unlinkat(root, "final", 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.RenameatxNp(root, "stage", root, "final", flags); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsync(root); err != nil {
		t.Fatal(err)
	}
	final, err := unix.Openat(root, "final", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(final)
	var after unix.Stat_t
	if err := unix.Fstat(final, &after); err != nil {
		t.Fatal(err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Btim != after.Btim ||
		before.Blocks != after.Blocks || after.Nlink != 2 {
		t.Fatalf("publication changed object/data allocation: before=%+v after=%+v", before, after)
	}
	actual := make([]byte, len(data))
	if n, err := unix.Pread(final, actual, 0); err != nil || n != len(data) || string(actual) != string(data) {
		t.Fatalf("published data = %q, %d, %v", actual, n, err)
	}
	t.Logf("same inode=%d generation=%d links=%d blocks=%d; Go/libSystem calls passed",
		after.Ino, after.Gen, after.Nlink, after.Blocks)
}
