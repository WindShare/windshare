//go:build !linux && !windows

package processrun

import (
	"context"
	"errors"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func startPlatform(context.Context, string, Spec, protocol.Request, *processOutput) (platformSession, error) {
	return nil, errors.New("process ownership is unsupported on this platform")
}
