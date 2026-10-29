//go:build windows || linux || darwin

package osfs

import (
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputportable"
)

// Receive and recovery must open the same backend on every supported host.
func openNativeOutputPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputportable.Open(path, create)
}
