//go:build windows

package statefile

import (
	"context"
	"errors"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows antivirus minifilters and lock-free concurrent readers can briefly
// hold a handle on either the freshly closed staging file or destination path,
// causing MoveFileExW to fail with ACCESS_DENIED or SHARING_VIOLATION. Keep
// total retry delay tightly bounded (315ms across 6 waits) because callers hold
// the statefile lock and permanent ACL errors also return ERROR_ACCESS_DENIED.
var windowsReplaceBackoff = [...]time.Duration{
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
}

func createPrivateFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	// The credential must be private at creation; chmod does not restrict a
	// Windows ACL, and tightening inherited access after writing leaves a gap.
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	security := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ,
		&security, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func replaceFile(ctx context.Context, source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return replaceFileOnWindows(ctx, func() error {
		return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}, waitWindowsReplace)
}

func replaceFileOnWindows(
	ctx context.Context,
	replace func() error,
	wait func(context.Context, time.Duration) error,
) error {
	err := replace()
	if err == nil || !isTransientWindowsReplaceError(err) {
		return err
	}
	for _, delay := range windowsReplaceBackoff {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if waitErr := wait(ctx, delay); waitErr != nil {
			return waitErr
		}
		err = replace()
		if err == nil || !isTransientWindowsReplaceError(err) {
			return err
		}
	}
	return err
}

func isTransientWindowsReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func waitWindowsReplace(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
