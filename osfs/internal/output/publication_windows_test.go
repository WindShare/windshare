//go:build windows

package output

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"golang.org/x/sys/windows"
)

func TestWindowsModifiedTimePreservesNativePrecisionAndHandleIdentity(t *testing.T) {
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
	if _, err := owned.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}

	var before windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(owned.Fd()), &before); err != nil {
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
	if err := setFileModifiedTimeCatalog(owned, modified); err != nil {
		t.Fatal(err)
	}
	if matches, err := fileMetadataMatches(owned, 4, modified); err != nil || !matches {
		t.Fatalf("native timestamp lost precision: matches=%t err=%v", matches, err)
	}
	var after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(owned.Fd()), &after); err != nil {
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

func TestRetryWindowsRenameBoundedBackoffPolicy(t *testing.T) {
	openSharing := &os.PathError{Op: "open", Path: "stage", Err: windows.ERROR_SHARING_VIOLATION}
	accessDenied := windows.ERROR_ACCESS_DENIED
	exist := errors.Join(ErrNamespaceCollision, fs.ErrExist, windows.ERROR_ALREADY_EXISTS)
	unsafeDenied := errors.Join(ErrUnsafeNamespace, windows.ERROR_ACCESS_DENIED)
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
			name:      "recovers on final attempt",
			results:   []error{openSharing, accessDenied, openSharing, accessDenied, openSharing, accessDenied, nil},
			wantWaits: allWaits,
		},
		{
			name:      "exhausts retries",
			results:   []error{accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, accessDenied, openSharing},
			wantErr:   openSharing,
			wantWaits: allWaits,
		},
		{name: "collision does not retry", results: []error{exist}, wantErr: exist},
		{name: "unsupported rename does not retry", results: []error{unsupported}, wantErr: unsupported},
		{name: "unsafe namespace does not retry", results: []error{unsafeDenied}, wantErr: unsafeDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			var waited []time.Duration
			err := retryWindowsRename(func() error {
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
