//go:build !linux && !windows

package osfs

import "fmt"

func openOutputV3Platform(_ string, _ bool) (outputV3Platform, error) {
	return nil, fmt.Errorf("%w: certified only on Linux/ext4 and Windows/local-NTFS", errOutputV3Unsupported)
}
