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

func TestResolvePOSIXTargetAndDirectoryEnforceAgreementAndLoopClassification(t *testing.T) {
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
	dirLink := filepath.Join(base, "dir-link")
	if err := os.Symlink(target, dirLink); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(base, "file-link")
	if err := os.Symlink(regularFile, fileLink); err != nil {
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
	linkLstat, err := os.Lstat(dirLink)
	if err != nil {
		t.Fatal(err)
	}

	// Real directory symlink resolves cleanly for both Target and Directory APIs.
	resolvedDir, info, err := ResolvePOSIXDirectory(dirLink, POSIXOps{})
	if err != nil || !os.SameFile(info, targetInfo) {
		t.Fatalf("ResolvePOSIXDirectory(dirLink) = (%q, %v, %v)", resolvedDir, info, err)
	}
	if wantDir, err := filepath.EvalSymlinks(target); err != nil || resolvedDir != wantDir {
		t.Fatalf("resolvedDir = %q, want %q (err=%v)", resolvedDir, wantDir, err)
	}

	// Real regular-file symlink resolves cleanly for ResolvePOSIXTarget and fails
	// fast with ErrNotDirectory for ResolvePOSIXDirectory without EvalSymlinks.
	resolvedFile, info, err := ResolvePOSIXTarget(fileLink, POSIXOps{})
	if err != nil || !os.SameFile(info, fileInfo) || !info.Mode().IsRegular() {
		t.Fatalf("ResolvePOSIXTarget(fileLink) = (%q, %v, %v)", resolvedFile, info, err)
	}
	if wantFile, err := filepath.EvalSymlinks(regularFile); err != nil || resolvedFile != wantFile {
		t.Fatalf("resolvedFile = %q, want %q (err=%v)", resolvedFile, wantFile, err)
	}
	evalCalled := false
	if _, _, err := ResolvePOSIXDirectory(fileLink, POSIXOps{
		EvalSymlinks: func(string) (string, error) {
			evalCalled = true
			return "", nil
		},
	}); !errors.Is(err, ErrNotDirectory) || evalCalled {
		t.Fatalf("ResolvePOSIXDirectory(fileLink) = (%v, evalCalled=%v), want ErrNotDirectory without EvalSymlinks", err, evalCalled)
	}

	// Simulated kernel EACCES (e.g. fs.protected_symlinks) propagates as permission error.
	if _, _, err := ResolvePOSIXTarget(dirLink, POSIXOps{
		Follow: func(string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: dirLink, Err: fs.ErrPermission}
		},
	}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission error = %v, want fs.ErrPermission", err)
	}

	// Kernel ELOOP maps to ErrSymlinkLoop.
	if _, _, err := ResolvePOSIXTarget(dirLink, POSIXOps{
		Follow: func(string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: dirLink, Err: syscall.ELOOP}
		},
	}); !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("kernel ELOOP = %v, want ErrSymlinkLoop", err)
	}

	// EvalSymlinks hitting a loop after Stat succeeds maps to ErrSymlinkLoop.
	if _, _, err := ResolvePOSIXTarget(dirLink, POSIXOps{
		Follow:       func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks: func(string) (string, error) { return "", errors.New("EvalSymlinks: too many links") },
	}); !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("EvalSymlinks loop = %v, want ErrSymlinkLoop", err)
	}

	// Lstat returning a symlink, another object, or a mismatched mode type maps to ErrTargetMismatch.
	for _, tc := range []struct {
		name  string
		lstat fs.FileInfo
	}{
		{name: "lstat symlink race", lstat: linkLstat},
		{name: "lstat swapped dir", lstat: otherInfo},
		{name: "lstat mode type mismatch", lstat: fileInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ResolvePOSIXTarget(dirLink, POSIXOps{
				Follow:       func(string) (fs.FileInfo, error) { return targetInfo, nil },
				EvalSymlinks: func(string) (string, error) { return target, nil },
				Lstat:        func(string) (fs.FileInfo, error) { return tc.lstat, nil },
			}); !errors.Is(err, ErrTargetMismatch) {
				t.Fatalf("%s = %v, want ErrTargetMismatch", tc.name, err)
			}
		})
	}
}
