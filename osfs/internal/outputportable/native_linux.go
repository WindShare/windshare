//go:build linux

package outputportable

import (
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func deviceID(stat *syscall.Stat_t) uint64 { return stat.Dev }

func setFileModifiedTime(handle *os.File, t time.Time) error {
	mtime, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	// The open handle remains authoritative after a rename or replacement;
	// an empty path also avoids the procfs dependency of unix.Futimes.
	times := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, mtime}
	err = unix.UtimesNanoAt(int(handle.Fd()), "", times, unix.AT_EMPTY_PATH)
	runtime.KeepAlive(handle)
	return err
}

func renameNoReplace(from int, old string, to int, name string) error {
	return noReplaceError(unix.Renameat2(from, old, to, name, unix.RENAME_NOREPLACE))
}
