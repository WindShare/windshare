//go:build linux

package checkpointcleaner

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func openCleanerTestPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}
