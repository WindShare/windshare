//go:build !windows

package windowsjob

import (
	"errors"
	"io"
	"os"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

func makeLauncherHandlePrivate(uintptr, string) error {
	return errors.New("Windows Job Object process ownership is unavailable on this platform")
}

func closeUntransferredLauncherHandle(uintptr) {}

func runSupervisorPlatform(supervisionRequest, *settlementSink, *os.File, *os.File, *startGate, io.Writer) error {
	return errors.New("windowsjob supervision is available only on Windows")
}

func runLauncherPlatform(ownerprotocol.Request, uintptr, uintptr, io.Reader) error {
	return errors.New("windowsjob launcher is available only on Windows")
}
