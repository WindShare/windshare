//go:build windows

package resumeauthority_test

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

func openNativeTestPlatform(path string) (outputcap.Platform, error) {
	return outputwindows.Open(path, false)
}
