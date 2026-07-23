//go:build linux

package osfs

import (
	"errors"
	"os"
	"syscall"
)

func platformMutationToken(file *os.File) (posixMutationToken, error) {
	information, err := file.Stat()
	if err != nil {
		return posixMutationToken{}, err
	}
	stat, ok := information.Sys().(*syscall.Stat_t)
	if !ok {
		return posixMutationToken{}, errors.New("Linux FileInfo does not expose syscall.Stat_t")
	}
	return posixMutationToken{
		device: uint64(stat.Dev), inode: stat.Ino, size: stat.Size,
		modifiedSec: int64(stat.Mtim.Sec), modifiedNS: int64(stat.Mtim.Nsec),
		changedSec: int64(stat.Ctim.Sec), changedNS: int64(stat.Ctim.Nsec),
	}, nil
}
