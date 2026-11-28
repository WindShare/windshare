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
	"github.com/windshare/windshare/osfs/internal/linkresolve"
)

func TestPOSIXForeignParentDisagreementIsUniformlyRefused(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real-parent")
	target := filepath.Join(base, "target")
	elsewhere := filepath.Join(base, "elsewhere", "child")
	for _, dir := range []string{realParent, target, elsewhere} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(realParent, "jump")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(target, "jump")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// Whether the logical parent is a real directory or a symlink, ".." must
	// fail closed when lexical $PWD/.. disagrees with physical kernel "..".
	t.Run("real logical parent", func(t *testing.T) {
		t.Chdir(filepath.Join(realParent, "jump"))
		assertUnsupportedSelection(t, []string{".."}, "..")
	})
	t.Run("symlinked logical parent", func(t *testing.T) {
		t.Chdir(filepath.Join(link, "jump"))
		assertUnsupportedSelection(t, []string{".."}, "..")
	})
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
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	// Simulated Linux fs.protected_symlinks EACCES refusal.
	refuse := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: ".", Err: fs.ErrPermission}
	}
	_, err = resolveLinkedWorkingDirectoryWith(".", link, linkresolve.POSIXOps{FollowSelected: refuse})
	failure, classified := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, fs.ErrPermission) || !classified || failure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("protected_symlinks refusal = %v (failure=%+v)", err, failure)
	}

	// Kernel ELOOP refusal maps to unsupported stability.
	loopErr := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: ".", Err: syscall.ELOOP}
	}
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, linkresolve.POSIXOps{FollowSelected: loopErr}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("ELOOP refusal = %v, want ErrUnsupportedStability", err)
	}

	// Kernel returning a non-directory or a different directory is refused.
	fileFollow := func(string) (fs.FileInfo, error) { return os.Stat(regularFile) }
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, linkresolve.POSIXOps{FollowSelected: fileFollow}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("non-directory follow = %v, want ErrUnsupportedStability", err)
	}
	otherDirFollow := func(string) (fs.FileInfo, error) { return os.Stat(base) }
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, linkresolve.POSIXOps{FollowSelected: otherDirFollow}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("disagreeing follow = %v, want ErrUnsupportedStability", err)
	}

	// EvalSymlinks hitting a symlink loop after Stat succeeds maps to unsupported stability.
	if _, err := resolveLinkedWorkingDirectoryWith(".", link, linkresolve.POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return targetInfo, nil },
		StatLogical:    func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks:   func(string) (string, error) { return "", errors.New("EvalSymlinks: too many links") },
	}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("EvalSymlinks loop = %v, want ErrUnsupportedStability", err)
	}

	// Direct parent-dot verification also maps ELOOP to unsupported stability and EACCES to permission.
	if err := verifyParentDotTraversalWith("..", target, sourceObjectIdentity{info: targetInfo}, loopErr); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("parent-dot ELOOP = %v, want ErrUnsupportedStability", err)
	}
	if err := verifyParentDotTraversalWith("..", target, sourceObjectIdentity{info: targetInfo}, refuse); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("parent-dot EACCES = %v, want fs.ErrPermission", err)
	}
}
