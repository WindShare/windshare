package platformsetup

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func sameInstallationPath(installed, current string) bool {
	installed, current = filepath.Clean(installed), filepath.Clean(current)
	if strings.EqualFold(installed, current) {
		return true
	}
	// Firewall rules follow a location across upgrades. Expand 8.3 aliases
	// without equating hard links at different installation locations.
	installed, err := longInstallationPath(installed)
	if err != nil {
		return false
	}
	current, err = longInstallationPath(current)
	return err == nil && strings.EqualFold(installed, current)
}

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

func Read() Status {
	config := os.Getenv("LOCALAPPDATA")
	if config == "" {
		return unavailable("config-directory-unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		return unavailable("executable-unavailable")
	}
	return ReadForExecutable(filepath.Join(config, "WindShare", "connectivity-setup.json"), executable)
}
