//go:build !windows

package osfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestPOSIXForeignParentThroughLinkedWorkingDirectoryIsRefused(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	elsewhere := filepath.Join(base, "elsewhere", "child")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(target, "jump")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// Logical ".." resolves to link (-> target), while kernel ".." from inside
	// target/jump resolves to elsewhere. Disagreement must fail closed.
	t.Chdir(filepath.Join(link, "jump"))
	assertUnsupportedSelection(t, []string{".."}, "..")
}

func TestPOSIXResolveLinkedWorkingDirectoryRespectsKernelPolicyAndLoops(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	regularFile := filepath.Join(target, "file.txt")
	if err := os.WriteFile(regularFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// Simulated Linux fs.protected_symlinks EACCES refusal.
	refuse := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: ".", Err: fs.ErrPermission}
	}
	_, err := resolveLinkedWorkingDirectoryWith(".", link, refuse)
	failure, classified := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, fs.ErrPermission) || !classified || failure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("protected_symlinks refusal = %v (failure=%+v)", err, failure)
	}

	// Kernel ELOOP refusal maps to unsupported stability.
	loopErr := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: ".", Err: syscall.ELOOP}
	}
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, loopErr); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("ELOOP refusal = %v, want ErrUnsupportedStability", err)
	}

	// Kernel returning a non-directory or a different directory is refused.
	fileFollow := func(string) (fs.FileInfo, error) { return os.Stat(regularFile) }
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, fileFollow); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("non-directory follow = %v, want ErrUnsupportedStability", err)
	}
	otherDirFollow := func(string) (fs.FileInfo, error) { return os.Stat(base) }
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, otherDirFollow); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("disagreeing follow = %v, want ErrUnsupportedStability", err)
	}

	// EvalSymlinks hitting a symlink loop maps to unsupported stability.
	loop := filepath.Join(base, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	targetFollow := func(string) (fs.FileInfo, error) { return os.Stat(target) }
	if _, err := resolveLinkedWorkingDirectoryWith(".", loop, targetFollow); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("symlink loop = %v, want ErrUnsupportedStability", err)
	}
}
