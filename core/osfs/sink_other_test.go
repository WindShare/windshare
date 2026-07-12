//go:build !windows

package osfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOSRootRejectsFinalSymlinkWithTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(dir, "root")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "escape")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	methods := []struct {
		name string
		open func() (io.Closer, error)
	}{
		{name: "Root.Open", open: func() (io.Closer, error) { return root.Open("escape/") }},
		{name: "Root.OpenFile", open: func() (io.Closer, error) { return root.OpenFile("escape/", os.O_RDONLY, 0) }},
		{name: "Root.OpenRoot", open: func() (io.Closer, error) { return root.OpenRoot("escape/") }},
		{name: "Root.FS.Open", open: func() (io.Closer, error) { return root.FS().Open("escape/") }},
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			opened, err := method.open()
			if err == nil {
				_ = opened.Close()
				t.Fatal("os.Root followed a final symlink with a trailing slash outside its root")
			}
		})
	}
}

func TestSinkRootCapabilityRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "secret")
	root := filepath.Join(dir, "out")
	s, err := NewSink(root, SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if err := s.EnsureDir("escape/newdir"); err == nil {
		t.Fatal("EnsureDir followed a symlink outside the root")
	}
	if err := s.WriteRange("escape/new.txt", 0, []byte("leak")); err == nil {
		t.Fatal("WriteRange followed a symlink outside the root")
	}
	if err := s.SetMTime("escape/secret.txt", time.Now().UnixMilli()); err == nil {
		t.Fatal("SetMTime followed a symlink outside the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file created through symlink: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
	if err != nil || string(got) != "secret" {
		t.Fatalf("outside content changed: %q, %v", got, err)
	}
}

func TestSinkRootCapabilitySurvivesRootPathReplacement(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "out")
	s, err := NewSink(root, SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	movedRoot := filepath.Join(dir, "opened-root")
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("cannot replace root path with symlink: %v", err)
	}

	if err := s.WriteRange("safe.txt", 0, []byte("safe")); err != nil {
		t.Fatalf("root-handle write after rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "safe.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write escaped through replacement root path: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(movedRoot, "safe.txt"))
	if err != nil || string(got) != "safe" {
		t.Fatalf("opened root did not retain the write: %q, %v", got, err)
	}
}

func TestReadRangeRejectsSymlinkReplacementWithMatchingMetadata(t *testing.T) {
	snap, path, osPath := snapshotSingleFile(t, "original!!")
	ref := snap.files[path]
	target := filepath.Join(filepath.Dir(osPath), "unrelated.bin")
	mustWriteFile(t, target, "unrelated!")
	matchingTime := time.UnixMilli(ref.mtime)
	if err := os.Chtimes(target, matchingTime, matchingTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(osPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, osPath); err != nil {
		t.Skipf("cannot create replacement symlink: %v", err)
	}
	fi, err := os.Stat(osPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != ref.size || fi.ModTime().UnixMilli() != ref.mtime {
		t.Skipf("filesystem cannot preserve the matching-metadata fixture: size=%d mtime=%d", fi.Size(), fi.ModTime().UnixMilli())
	}

	if _, err := NewSource(snap).ReadRange(path, 0, ref.size); !errors.Is(err, ErrDrift) {
		t.Fatalf("symlink replacement with matching metadata: err = %v, want ErrDrift", err)
	}
}
