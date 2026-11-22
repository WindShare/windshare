//go:build windows

package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestRejectedRenameOperationIsDefiniteUnsupportedPublication(t *testing.T) {
	for _, cause := range []error{windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION, windows.ERROR_CALL_NOT_IMPLEMENTED} {
		outcome, err := publicationResult(noReplaceError(cause))
		if outcome != 0 || !errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) || !errors.Is(err, cause) {
			t.Fatalf("unsupported rename = %s, %v; want no mutation and unsupported", outcome, err)
		}
	}
	outcome, err := publicationResult(noReplaceError(windows.ERROR_IO_DEVICE))
	if outcome != outputcap.PublishNoReplaceIndeterminate || !errors.Is(err, windows.ERROR_IO_DEVICE) ||
		errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		t.Fatalf("I/O failure lost uncertainty: %s, %v", outcome, err)
	}
}

func TestRetryWindowsRenameBoundedBackoffPolicy(t *testing.T) {
	openSharing := &os.PathError{Op: "open", Path: "stage", Err: windows.ERROR_SHARING_VIOLATION}
	accessDenied := windows.ERROR_ACCESS_DENIED
	exist := errors.Join(fs.ErrExist, windows.ERROR_ALREADY_EXISTS)
	unsafeDenied := errors.Join(outputcap.ErrUnsafeNamespace, windows.ERROR_ACCESS_DENIED)
	unsupported := noReplaceError(windows.ERROR_NOT_SUPPORTED)
	allWaits := windowsRenameBackoff[:]

	for _, tc := range []struct {
		name      string
		results   []error
		wantErr   error
		wantWaits []time.Duration
	}{
		{name: "immediate success", results: []error{nil}},
		{name: "recovers after open sharing violation", results: []error{openSharing, nil}, wantWaits: allWaits[:1]},
		{name: "recovers after access denied", results: []error{accessDenied, nil}, wantWaits: allWaits[:1]},
		{
			name:      "recovers on final attempt after full scan window",
			results:   []error{openSharing, accessDenied, openSharing, accessDenied, openSharing, accessDenied, nil},
			wantWaits: allWaits,
		},
		{
			name:      "exhausts retries and returns final transient error",
			results:   []error{accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, openSharing},
			wantErr:   openSharing,
			wantWaits: allWaits,
		},
		{name: "collision does not retry", results: []error{exist}, wantErr: exist},
		{name: "unsupported rename does not retry", results: []error{unsupported}, wantErr: unsupported},
		{name: "unsafe namespace with access denied does not retry", results: []error{unsafeDenied}, wantErr: unsafeDenied},
		{
			name:      "stops when retry hits non-transient error",
			results:   []error{openSharing, exist},
			wantErr:   exist,
			wantWaits: allWaits[:1],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			var waited []time.Duration
			err := retryWindowsRename(func() error {
				if attempts >= len(tc.results) {
					t.Fatalf("unexpected retry attempt %d", attempts+1)
				}
				result := tc.results[attempts]
				attempts++
				return result
			}, func(delay time.Duration) {
				waited = append(waited, delay)
			})
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("retryWindowsRename = %v, want %v", err, tc.wantErr)
			}
			if attempts != len(tc.results) || !slices.Equal(waited, tc.wantWaits) {
				t.Fatalf("attempts=%d waits=%v, want %d and %v", attempts, waited, len(tc.results), tc.wantWaits)
			}
		})
	}
}

func openForeignNoDeleteHandle(t *testing.T, path string) *os.File {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("open foreign handle %q: %v", path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestRenameFileRetriesTransientSourceContention(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	dir := platform.Root().(*directory)
	staged, err := dir.CreateFile("stage.part", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.WriteAt([]byte("done"), 0); err != nil {
		_ = staged.Close()
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen read-only so a user-mode reader handle without FILE_SHARE_DELETE can
	// share the source file, reproducing scanner contention during rename open.
	observed, err := dir.OpenObservedFile("stage.part", false)
	if err != nil {
		t.Fatal(err)
	}
	defer observed.Close()

	foreign := openForeignNoDeleteHandle(t, filepath.Join(root, "stage.part"))
	var waited []time.Duration
	err = renameFileWithWait(observed.(*file), dir, "final.bin", false, func(delay time.Duration) {
		waited = append(waited, delay)
		if closeErr := foreign.Close(); closeErr != nil {
			t.Errorf("close foreign handle: %v", closeErr)
		}
	})
	if err != nil {
		t.Fatalf("renameFileWithWait = %v, want nil", err)
	}
	if !slices.Equal(waited, windowsRenameBackoff[:1]) {
		t.Fatalf("waited = %v, want %v", waited, windowsRenameBackoff[:1])
	}
	if err := observed.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "final.bin"))
	if err != nil || string(content) != "done" {
		t.Fatalf("published content = %q, %v; want done", content, err)
	}
}

func TestRenameFileReplaceRetriesTransientDestinationContention(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	dir := platform.Root().(*directory)
	if err := os.WriteFile(filepath.Join(root, "record"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := dir.CreateFile("candidate", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if _, err := candidate.WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}

	foreign := openForeignNoDeleteHandle(t, filepath.Join(root, "record"))
	var waited []time.Duration
	err = renameFileWithWait(candidate.(*file), dir, "record", true, func(delay time.Duration) {
		waited = append(waited, delay)
		if closeErr := foreign.Close(); closeErr != nil {
			t.Errorf("close foreign handle: %v", closeErr)
		}
	})
	if err != nil {
		t.Fatalf("renameFileWithWait(replace=true) = %v, want nil", err)
	}
	if !slices.Equal(waited, windowsRenameBackoff[:1]) {
		t.Fatalf("waited = %v, want %v", waited, windowsRenameBackoff[:1])
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "record"))
	if err != nil || string(content) != "new" {
		t.Fatalf("replaced content = %q, %v; want new", content, err)
	}
}

func TestRenameFileRetryRevalidatesSourceIdentity(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	dir := platform.Root().(*directory)
	staged, err := dir.CreateFile("stage.part", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.WriteAt([]byte("mine"), 0); err != nil {
		_ = staged.Close()
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	observed, err := dir.OpenObservedFile("stage.part", false)
	if err != nil {
		t.Fatal(err)
	}
	defer observed.Close()

	stagePath := filepath.Join(root, "stage.part")
	foreign := openForeignNoDeleteHandle(t, stagePath)
	var waited []time.Duration
	err = renameFileWithWait(observed.(*file), dir, "final.bin", false, func(delay time.Duration) {
		waited = append(waited, delay)
		if closeErr := foreign.Close(); closeErr != nil {
			t.Errorf("close foreign handle: %v", closeErr)
		}
		// Swap the source name with an unrelated file before the retry opens it.
		// Displacing the open stage avoids NTFS delete-pending rejection when
		// recreating the same path while observed remains open.
		if displaceErr := os.Rename(stagePath, filepath.Join(root, "stage.displaced")); displaceErr != nil {
			t.Errorf("displace staged source: %v", displaceErr)
		}
		if writeErr := os.WriteFile(stagePath, []byte("other"), 0o600); writeErr != nil {
			t.Errorf("plant foreign source: %v", writeErr)
		}
	})
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("renameFileWithWait after source swap = %v, want ErrUnsafeNamespace", err)
	}
	if !slices.Equal(waited, windowsRenameBackoff[:1]) {
		t.Fatalf("waited = %v, want %v", waited, windowsRenameBackoff[:1])
	}
	if _, statErr := os.Stat(filepath.Join(root, "final.bin")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("swapped source was published to final.bin: %v", statErr)
	}
}

func TestRenameDirectoryRetriesTransientChildHandleContention(t *testing.T) {
	root := t.TempDir()
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	dir := platform.Root().(*directory)
	candidate, err := dir.CreateDirectory("staged-dir", true)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	childPath := filepath.Join(root, "staged-dir", "child.bin")
	if err := os.WriteFile(childPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	foreign := openForeignNoDeleteHandle(t, childPath)
	var waited []time.Duration
	err = renameDirectoryWithWait(candidate.(*directory), dir, "installed-dir", func(delay time.Duration) {
		waited = append(waited, delay)
		if closeErr := foreign.Close(); closeErr != nil {
			t.Errorf("close foreign child handle: %v", closeErr)
		}
	})
	if err != nil {
		t.Fatalf("renameDirectoryWithWait = %v, want nil", err)
	}
	if !slices.Equal(waited, windowsRenameBackoff[:1]) {
		t.Fatalf("waited = %v, want %v", waited, windowsRenameBackoff[:1])
	}
	if _, err := os.Stat(filepath.Join(root, "installed-dir", "child.bin")); err != nil {
		t.Fatalf("installed directory missing child: %v", err)
	}
}
