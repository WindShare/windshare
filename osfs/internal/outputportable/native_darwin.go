//go:build darwin

package outputportable

import (
	"encoding/binary"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func directoryRecoveryStamp(f *os.File) ([]byte, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return nil, err
	}
	return darwinCreationStamp(stat)
}

func darwinCreationStamp(stat unix.Stat_t) ([]byte, error) {
	return creationStamp(stat.Btim.Sec, stat.Btim.Nsec)
}

func setDirectoryTimes(d *directory, t time.Time) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(d.handle.Fd()), &stat); err != nil {
		return err
	}
	// Darwin may lower birth time when mtime is moved backwards. Set creation
	// and modification times together so source metadata cannot revoke recovery.
	attributes := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr: unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	var encoded []byte
	for _, stamp := range []unix.Timespec{stat.Btim, unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())} {
		encoded = binary.NativeEndian.AppendUint64(encoded, uint64(stamp.Sec))
		encoded = binary.NativeEndian.AppendUint64(encoded, uint64(stamp.Nsec))
	}
	return unix.Setattrlist(d.path, &attributes, encoded, unix.FSOPT_NOFOLLOW)
}

func renameNoReplace(from int, old string, to int, name string) error {
	return noReplaceError(unix.RenameatxNp(from, old, to, name, unix.RENAME_EXCL))
}
