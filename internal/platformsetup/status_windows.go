package platformsetup

import (
	"os"
	"path/filepath"
)

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
