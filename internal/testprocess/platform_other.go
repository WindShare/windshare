//go:build !linux && !windows

package testprocess

import (
	"context"
	"errors"
)

func newPlatformCommand(context.Context, string, string, []byte) (*platformCommand, error) {
	return nil, errors.New("test process ownership supports only Linux and Windows")
}
