package dirpublish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRenameOnWindowsRetriesTransientErrorsWithBoundedBackoff(t *testing.T) {
	denied := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_ACCESS_DENIED}
	sharing := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_SHARING_VIOLATION}
	missing := &os.LinkError{Op: "rename", Old: "staged", New: "published", Err: windows.ERROR_PATH_NOT_FOUND}
	allWaits := windowsRenameBackoff[:]
	for _, test := range []struct {
		name           string
		results        []error
		cancelOnCall   int
		cancelDuringOp bool
		want           error
		wantWaits      []time.Duration
	}{
		{name: "success", results: []error{nil}},
		{name: "access restored", results: []error{denied, nil}, wantWaits: allWaits[:1]},
		{name: "sharing restored", results: []error{sharing, nil}, wantWaits: allWaits[:1]},
		{
			name:      "restored on final attempt after defender scan window",
			results:   []error{denied, sharing, denied, sharing, denied, sharing, nil},
			wantWaits: allWaits,
		},
		{
			name:      "exhausted transient retries",
			results:   []error{denied, denied, denied, denied, denied, denied, sharing},
			want:      sharing,
			wantWaits: allWaits,
		},
		{
			name:      "non-transient error stops retries",
			results:   []error{denied, missing},
			want:      missing,
			wantWaits: allWaits[:1],
		},
		{name: "missing path", results: []error{missing}, want: missing},
		{
			name:         "canceled before backoff wait",
			results:      []error{sharing},
			cancelOnCall: 1,
			want:         context.Canceled,
		},
		{
			name:           "canceled during backoff wait",
			results:        []error{denied},
			cancelDuringOp: true,
			want:           context.Canceled,
			wantWaits:      allWaits[:1],
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			var waits []time.Duration
			err := renameOnWindows(ctx, func() error {
				if calls >= len(test.results) {
					t.Fatal("unexpected extra rename")
				}
				result := test.results[calls]
				calls++
				if test.cancelOnCall == calls {
					cancel()
				}
				return result
			}, func(waitCtx context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				if test.cancelDuringOp {
					cancel()
					return waitCtx.Err()
				}
				return nil
			})
			if err != test.want || calls != len(test.results) || !slices.Equal(waits, test.wantWaits) {
				t.Fatalf(
					"rename returned %v after %d calls (waits=%v), want %v after %d (waits=%v)",
					err, calls, waits, test.want, len(test.results), test.wantWaits,
				)
			}
		})
	}
}

func TestWaitWindowsRenameHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitWindowsRename(ctx, time.Minute); err != context.Canceled {
		t.Fatalf("waitWindowsRename = %v, want context.Canceled", err)
	}
}

func TestSyncDirIsNoOpOnWindows(t *testing.T) {
	// Passing a nonexistent path verifies SyncDir performs no pointless os.Open
	// or failing FlushFileBuffers syscall on Windows.
	if err := SyncDir(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatalf("SyncDir on Windows = %v, want nil", err)
	}
}
