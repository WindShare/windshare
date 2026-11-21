//go:build windows

package statefile

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReplaceFileOnWindowsRetriesTransientErrorsWithBackoff(t *testing.T) {
	failures := []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED}
	attempts := 0
	var waited []time.Duration
	err := replaceFileOnWindows(
		t.Context(),
		func() error {
			attempts++
			if attempts <= len(failures) {
				return failures[attempts-1]
			}
			return nil
		},
		func(_ context.Context, delay time.Duration) error {
			waited = append(waited, delay)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("replaceFileOnWindows returned %v, want nil", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	wantWaits := windowsReplaceBackoff[:2]
	if !slices.Equal(waited, wantWaits) {
		t.Fatalf("waited = %v, want %v", waited, wantWaits)
	}
}

func TestReplaceFileOnWindowsFailsImmediatelyOnNonTransientError(t *testing.T) {
	attempts := 0
	waits := 0
	err := replaceFileOnWindows(
		t.Context(),
		func() error {
			attempts++
			return windows.ERROR_FILE_NOT_FOUND
		},
		func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	)
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("err = %v, want ERROR_FILE_NOT_FOUND", err)
	}
	if attempts != 1 || waits != 0 {
		t.Fatalf("attempts=%d waits=%d, want 1 and 0", attempts, waits)
	}
}

func TestReplaceFileOnWindowsStopsAfterBoundedBackoff(t *testing.T) {
	attempts := 0
	var waited []time.Duration
	err := replaceFileOnWindows(
		t.Context(),
		func() error {
			attempts++
			return windows.ERROR_ACCESS_DENIED
		},
		func(_ context.Context, delay time.Duration) error {
			waited = append(waited, delay)
			return nil
		},
	)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("err = %v, want ERROR_ACCESS_DENIED", err)
	}
	if attempts != len(windowsReplaceBackoff)+1 {
		t.Fatalf("attempts = %d, want %d", attempts, len(windowsReplaceBackoff)+1)
	}
	if !slices.Equal(waited, windowsReplaceBackoff[:]) {
		t.Fatalf("waited = %v, want %v", waited, windowsReplaceBackoff[:])
	}
}

func TestReplaceFileOnWindowsHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	err := replaceFileOnWindows(
		ctx,
		func() error {
			attempts++
			cancel()
			return windows.ERROR_SHARING_VIOLATION
		},
		func(context.Context, time.Duration) error {
			t.Fatal("wait called after context cancellation")
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
