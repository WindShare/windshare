//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func resumeFileIsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(file.Fd()), &mode) == nil
}
