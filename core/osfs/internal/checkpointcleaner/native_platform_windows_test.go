//go:build windows

package checkpointcleaner

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

func openCleanerTestPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.Open(path, create)
}
