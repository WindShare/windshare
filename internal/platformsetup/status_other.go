//go:build !windows

package platformsetup

import "path/filepath"

func sameInstallationPath(installed, current string) bool {
	return filepath.Clean(installed) == filepath.Clean(current)
}

func Read() Status { return unavailable("platform-firewall-setup-unsupported") }
