//go:build !linux && !windows

package osfs

import (
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func openOutputV3Platform(_ string, _ bool) (outputcap.Platform, error) {
	return nil, fmt.Errorf("%w: certified only on Linux/ext4 and Windows/local-NTFS", outputcap.ErrRecoverableOutputUnsupported)
}
