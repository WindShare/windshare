//go:build !windows

package dirpublish

import (
	"context"
	"os"
)

func Rename(_ context.Context, staged, target string) error {
	return os.Rename(staged, target)
}
