//go:build !windows && !linux

package testprocess

import (
	"context"
	"errors"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func startPlatform(context.Context, string, Spec, protocol.Request, *processOutput) (platformSession, error) {
	return nil, errors.New("test process ownership is supported only on Windows and Linux")
}
