//go:build windows

package osfs

import (
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputwindows"
)

func openOutputV3Platform(path string, create bool) (outputcap.Platform, error) {
	return outputwindows.Open(path, create)
}
