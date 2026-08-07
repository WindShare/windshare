//go:build !windows && !linux

package checkpointcleaner

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func openCleanerTestPlatform(string, bool) (outputcap.Platform, error) {
	return nil, outputcap.ErrRecoverableOutputUnsupported
}
