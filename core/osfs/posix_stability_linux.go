//go:build linux

package osfs

import (
	"errors"
	"os"
	"syscall"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputlinux"
)

func platformMutationToken(file *os.File) (posixMutationToken, error) {
	information, err := file.Stat()
	if err != nil {
		return posixMutationToken{}, err
	}
	stat, ok := information.Sys().(*syscall.Stat_t)
	if !ok {
		return posixMutationToken{}, errors.New("file info does not expose syscall.Stat_t on Linux")
	}
	// syscall.Stat_t field widths differ across Linux architectures. These
	// conversions are redundant on amd64 but required by narrower ABIs.
	//nolint:unconvert
	return posixMutationToken{
		device: uint64(stat.Dev), inode: stat.Ino, size: stat.Size,
		modifiedSec: int64(stat.Mtim.Sec), modifiedNS: int64(stat.Mtim.Nsec),
		changedSec: int64(stat.Ctim.Sec), changedNS: int64(stat.Ctim.Nsec),
	}, nil
}

func openNativeOutputPlatform(path string, create bool) (outputcap.Platform, error) {
	return outputlinux.Open(path, create)
}
