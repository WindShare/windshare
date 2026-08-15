//go:build darwin

package osfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func platformMutationToken(file *os.File) (posixMutationToken, error) {
	information, err := file.Stat()
	if err != nil {
		return posixMutationToken{}, err
	}
	stat, ok := information.Sys().(*syscall.Stat_t)
	if !ok {
		return posixMutationToken{}, errors.New("file info does not expose syscall.Stat_t on Darwin")
	}
	return posixMutationToken{
		device: uint64(stat.Dev), inode: stat.Ino, size: stat.Size,
		modifiedSec: stat.Mtimespec.Sec, modifiedNS: stat.Mtimespec.Nsec,
		changedSec: stat.Ctimespec.Sec, changedNS: stat.Ctimespec.Nsec,
	}, nil
}

func openNativeOutputPlatform(_ string, _ bool) (outputcap.Platform, error) {
	return nil, fmt.Errorf("%w: certified only on Linux/ext4 and Windows/local-NTFS", outputcap.ErrRecoverableOutputUnsupported)
}
