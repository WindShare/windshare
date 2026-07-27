//go:build !linux && !windows

package cli

import "os"

func resumeFileIsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	information, err := file.Stat()
	return err == nil && information.Mode()&os.ModeCharDevice != 0
}
