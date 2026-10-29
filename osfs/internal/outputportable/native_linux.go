//go:build linux

package outputportable

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func deviceID(stat *syscall.Stat_t) uint64 { return stat.Dev }

func renameNoReplace(from int, old string, to int, name string) error {
	return noReplaceError(unix.Renameat2(from, old, to, name, unix.RENAME_NOREPLACE))
}
