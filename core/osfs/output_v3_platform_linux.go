//go:build linux

package osfs

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func openNativeOutputPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}
