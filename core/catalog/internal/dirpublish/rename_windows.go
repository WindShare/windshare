package dirpublish

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func Rename(ctx context.Context, staged, target string) error {
	return renameOnWindows(ctx, func() error { return os.Rename(staged, target) })
}

func renameOnWindows(ctx context.Context, rename func() error) error {
	err := rename()
	if err == nil || (!errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION)) {
		return err
	}
	// A handle below the staged directory can briefly prevent publication.
	// Keep the same prepared transaction and bound the time spent under its lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	return rename()
}
