package dirpublish

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Windows antivirus and search-indexer minifilters routinely hold handles
// beneath a freshly closed staged directory for 10-200ms, causing MoveFileExW
// to return ACCESS_DENIED or SHARING_VIOLATION. Total wait stays tightly
// bounded (315ms across 6 waits) because callers hold the backend publication
// lock and permanent ACL errors also surface as ERROR_ACCESS_DENIED.
var windowsRenameBackoff = [...]time.Duration{
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
}

func Rename(ctx context.Context, staged, target string) error {
	return renameOnWindows(ctx, func() error { return os.Rename(staged, target) }, waitWindowsRename)
}

func renameOnWindows(
	ctx context.Context,
	rename func() error,
	wait func(context.Context, time.Duration) error,
) error {
	err := rename()
	if err == nil || !isTransientWindowsRenameError(err) {
		return err
	}
	for _, delay := range windowsRenameBackoff {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if waitErr := wait(ctx, delay); waitErr != nil {
			return waitErr
		}
		err = rename()
		if err == nil || !isTransientWindowsRenameError(err) {
			return err
		}
	}
	return err
}

func isTransientWindowsRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func waitWindowsRename(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
