package platformsetup

import (
	"golang.org/x/sys/windows"
)

// A path, including its long Windows spelling, owns the rules across upgrades.
// Hard links at different paths must not acquire each other's rule identity.
func longInstallationPath(path string) (string, error) {
	input, err := windows.UTF16FromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, len(input))
	for {
		size, err := windows.GetLongPathName(&input[0], &buffer[0], uint32(len(buffer)))
		if err != nil {
			return "", err
		}
		if size < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:size]), nil
		}
		buffer = make([]uint16, size)
	}
}
