//go:build darwin

package outputportable

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func deviceID(stat *syscall.Stat_t) uint64 { return uint64(stat.Dev) }

func renameNoReplace(from int, old string, to int, name string) error {
	return unix.RenameatxNp(from, old, to, name, unix.RENAME_EXCL)
}
