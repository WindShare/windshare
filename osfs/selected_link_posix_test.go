//go:build linux || darwin

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
	"golang.org/x/sys/unix"
)

func TestPOSIXSelectedSymlinkToFIFOIsRejectedWithoutBlocking(t *testing.T) {
	base := t.TempDir()
	pipePath := filepath.Join(base, "named.pipe")
	if err := unix.Mkfifo(pipePath, 0o600); err != nil {
		t.Fatalf("Mkfifo failed: %v", err)
	}
	link := filepath.Join(base, "pipe-link")
	if err := os.Symlink(pipePath, link); err != nil {
		t.Fatal(err)
	}
	assertSelectionFailure(t, []string{link}, link, content.ErrUnsupportedStability, catalog.SourceFailureUnsupported)
}

func TestPOSIXResolveSelectedRootLinkPolicyAndRaces(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	regularFile := filepath.Join(target, "file.txt")
	if err := os.WriteFile(regularFile, []byte("file"), 0o600); err != nil {
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
	otherInfo, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}

	// Regular-file symlink resolves cleanly with sourceEntryRegularFile.
	resolvedFile, err := resolveSelectedRootLinkWith(fileLink, linkresolve.POSIXOps{})
	if err != nil || resolvedFile.disposition != selectedLinkResolved || resolvedFile.kind != sourceEntryRegularFile {
		t.Fatalf("resolveSelectedRootLinkWith(fileLink) = (%+v, %v)", resolvedFile, err)
	}

	// Simulated Linux fs.protected_symlinks EACCES refusal on stat(path).
	refuse := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: dirLink, Err: fs.ErrPermission}
	}
	_, err = resolveSelectedRootLinkWith(dirLink, linkresolve.POSIXOps{Follow: refuse})
	failure, classified := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, fs.ErrPermission) || !classified || failure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("protected_symlinks refusal = %v (failure=%+v)", err, failure)
	}

	// Kernel ELOOP refusal maps to unsupported stability.
	loopErr := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: dirLink, Err: syscall.ELOOP}
	}
	if _, err := resolveSelectedRootLinkWith(dirLink, linkresolve.POSIXOps{Follow: loopErr}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("ELOOP refusal = %v, want ErrUnsupportedStability", err)
	}

	// EvalSymlinks hitting a symlink loop after Stat succeeds maps to unsupported stability.
	if _, err := resolveSelectedRootLinkWith(dirLink, linkresolve.POSIXOps{
		Follow:       func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks: func(string) (string, error) { return "", errors.New("EvalSymlinks: too many links") },
	}); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("EvalSymlinks loop = %v, want ErrUnsupportedStability", err)
	}

	// Concurrent swap between Follow and Lstat maps to SourceFailureStale.
	_, err = resolveSelectedRootLinkWith(dirLink, linkresolve.POSIXOps{
		Follow:       func(string) (fs.FileInfo, error) { return targetInfo, nil },
		EvalSymlinks: func(string) (string, error) { return target, nil },
		Lstat:        func(string) (fs.FileInfo, error) { return otherInfo, nil },
	})
	staleFailure, staleOK := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, catalog.ErrDirectoryStale) || !staleOK || staleFailure.Failure != catalog.SourceFailureStale {
		t.Fatalf("TOCTOU swap = %v (failure=%+v), want ErrDirectoryStale / SourceFailureStale", err, staleFailure)
	}
}
