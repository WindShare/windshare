//go:build linux

package resumeauthority_test

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func openNativeTestPlatform(path string) (outputcap.Platform, error) {
	return outputlinux.Open(path, false)
}
