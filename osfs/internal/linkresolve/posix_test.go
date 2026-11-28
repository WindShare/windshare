//go:build !windows

package linkresolve

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolvePOSIXDirectoryEnforcesAgreementAndLoopClassification(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	other := filepath.Join(base, "other")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	regularFile := filepath.Join(target, "file.txt")
	if err := os.WriteFile(regularFile, []byte("ok"), 0o600); err != nil {
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
	otherInfo, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(regularFile)
	if err != nil {
		t.Fatal(err)
	}
	linkLstat, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}

	resolved, info, err := ResolvePOSIXDirectory(link, link, POSIXOps{})
	if err != nil || !os.SameFile(info, targetInfo) {
		t.Fatalf("ResolvePOSIXDirectory(link) = (%q, %v, %v)", resolved, info, err)
	}
	if resolvedTarget, err := filepath.EvalSymlinks(target); err != nil || resolved != resolvedTarget {
		t.Fatalf("resolved = %q, want %q (err=%v)", resolved, resolvedTarget, err)
	}

	// Simulated kernel EACCES (e.g. fs.protected_symlinks) propagates as permission error.
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: ".", Err: fs.ErrPermission}
		},
	}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission error = %v, want fs.ErrPermission", err)
	}

	// Kernel ELOOP maps to ErrSymlinkLoop.
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: ".", Err: syscall.ELOOP}
		},
	}); !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("kernel ELOOP = %v, want ErrSymlinkLoop", err)
	}

	// Non-directory kernel target maps to ErrNotDirectory.
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return fileInfo, nil },
	}); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("non-directory target = %v, want ErrNotDirectory", err)
	}

	// Disagreement between selectedPath and logicalPath maps to ErrDirectoryMismatch.
	if _, _, err := ResolvePOSIXDirectory("..", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return otherInfo, nil },
		StatLogical:    func(string) (fs.FileInfo, error) { return targetInfo, nil },
	}); !errors.Is(err, ErrDirectoryMismatch) {
		t.Fatalf("logical/kernel mismatch = %v, want ErrDirectoryMismatch", err)
	}

	// EvalSymlinks hitting a loop after Stat succeeds maps to ErrSymlinkLoop.
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return targetInfo, nil },
		StatLogical:    func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks:   func(string) (string, error) { return "", errors.New("EvalSymlinks: too many links") },
	}); !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("EvalSymlinks loop = %v, want ErrSymlinkLoop", err)
	}

	// Lstat returning a symlink or a different directory maps to ErrDirectoryMismatch.
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return targetInfo, nil },
		StatLogical:    func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks:   func(string) (string, error) { return target, nil },
		Lstat:          func(string) (fs.FileInfo, error) { return linkLstat, nil },
	}); !errors.Is(err, ErrDirectoryMismatch) {
		t.Fatalf("Lstat symlink race = %v, want ErrDirectoryMismatch", err)
	}
	if _, _, err := ResolvePOSIXDirectory(".", link, POSIXOps{
		FollowSelected: func(string) (fs.FileInfo, error) { return targetInfo, nil },
		StatLogical:    func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks:   func(string) (string, error) { return target, nil },
		Lstat:          func(string) (fs.FileInfo, error) { return otherInfo, nil },
	}); !errors.Is(err, ErrDirectoryMismatch) {
		t.Fatalf("Lstat swapped dir = %v, want ErrDirectoryMismatch", err)
	}
}

func TestVerifyPOSIXDirectoryAgreement(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	if err := os.Mkdir(dirA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirB, 0o700); err != nil {
		t.Fatal(err)
	}
	infoA, err := os.Stat(dirA)
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := os.Stat(dirB)
	if err != nil {
		t.Fatal(err)
	}

	if err := VerifyPOSIXDirectoryAgreement("..", infoA, func(string) (fs.FileInfo, error) { return infoA, nil }); err != nil {
		t.Fatalf("matching directory rejected: %v", err)
	}
	if err := VerifyPOSIXDirectoryAgreement("..", infoA, func(string) (fs.FileInfo, error) { return infoB, nil }); !errors.Is(err, ErrDirectoryMismatch) {
		t.Fatalf("mismatching directory = %v, want ErrDirectoryMismatch", err)
	}
	if err := VerifyPOSIXDirectoryAgreement("..", infoA, func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: "..", Err: syscall.ELOOP}
	}); !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("ELOOP = %v, want ErrSymlinkLoop", err)
	}
	if err := VerifyPOSIXDirectoryAgreement("..", infoA, func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: "..", Err: fs.ErrPermission}
	}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("EACCES = %v, want fs.ErrPermission", err)
	}
}
