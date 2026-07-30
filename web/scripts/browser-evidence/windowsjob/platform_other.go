//go:build !windows

package main

import (
	"errors"
	"io"
)

func runSupervisorPlatform(startRequest, string, string, io.Reader) error {
	return errors.New("windowsjob supervision is available only on Windows")
}

func runLauncherPlatform(startRequest, uintptr, uintptr, io.Reader) error {
	return errors.New("windowsjob launcher is available only on Windows")
}
