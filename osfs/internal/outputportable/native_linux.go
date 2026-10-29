//go:build linux

package outputportable

import (
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func directoryRecoveryStamp(f *os.File) ([]byte, error) {
	var stat unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stat); err != nil {
		return nil, err
	}
	if stat.Mask&unix.STATX_BTIME != 0 {
		return linuxCreationStamp(stat)
	}
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &filesystem); err != nil {
		return nil, err
	}
	if filesystem.Type == unix.TMPFS_MAGIC || filesystem.Type == unix.RAMFS_MAGIC {
		// Memory filesystems retain inode identity for their whole data lifetime;
		// unmounting their last reference destroys the data as well as the IDs.
		var current unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &current); err != nil {
			return nil, err
		}
		stamp := binary.BigEndian.AppendUint64([]byte("memory-directory/v1"), uint64(current.Dev))
		return binary.BigEndian.AppendUint64(stamp, current.Ino), nil
	}
	return linuxCreationStamp(stat)
}

func setDirectoryTimes(d *directory, t time.Time) error { return d.root.Chtimes(".", t, t) }

func linuxCreationStamp(stat unix.Statx_t) ([]byte, error) {
	if stat.Mask&unix.STATX_BTIME == 0 {
		return nil, errors.Join(outputcap.ErrRecoverableOutputUnsupported, errors.New("filesystem does not expose creation metadata"))
	}
	return creationStamp(stat.Btime.Sec, int64(stat.Btime.Nsec))
}

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
