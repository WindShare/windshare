//go:build darwin

package output

import (
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin's sys/stat.h defines UTIME_OMIT as -2; x/sys does not export it.
const darwinUtimeOmit = -2

func setFileModifiedTime(handle *os.File, t time.Time) error {
	if handle == nil {
		return fs.ErrClosed
	}
	mtime, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	times := [2]unix.Timespec{{Nsec: darwinUtimeOmit}, mtime}
	_, _, errno := timestampLibSystemCall6(futimensAddress, handle.Fd(),
		uintptr(unsafe.Pointer(&times)), 0, 0, 0, 0)
	runtime.KeepAlive(&times)
	runtime.KeepAlive(handle)
	if errno != 0 {
		return errno
	}
	return nil
}

func setDirModifiedTime(d *Dir, t time.Time) error {
	if d == nil || d.handle == nil {
		return fs.ErrClosed
	}
	mtime, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(d.handle.Fd()), &stat); err != nil {
		runtime.KeepAlive(d.handle)
		return err
	}
	// Darwin may lower birth time when mtime is moved backwards. Set creation
	// and modification times together via fsetattrlist so the update stays
	// anchored to the open directory handle and leaves atime untouched.
	attributes := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME,
	}
	times := [2]unix.Timespec{stat.Btim, mtime}
	// fsetattrlist operates on an open fd; path-only flags like FSOPT_NOFOLLOW
	// are rejected with EINVAL.
	_, _, errno := timestampLibSystemCall6(
		fsetattrlistAddress,
		d.handle.Fd(),
		uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&times)),
		unsafe.Sizeof(times),
		0,
		0,
	)
	runtime.KeepAlive(&attributes)
	runtime.KeepAlive(&times)
	runtime.KeepAlive(d.handle)
	if errno != 0 {
		return errno
	}
	return nil
}

// x/sys exposes only microsecond futimes and path-based Setattrlist on Darwin.
// Use its libSystem calling convention to preserve nanoseconds and handle
// identity without requiring CGO.
//
//go:linkname timestampLibSystemCall6 syscall.syscall6
//go:uintptrescapes
func timestampLibSystemCall6(function, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, errno syscall.Errno)

var (
	futimensAddress     uintptr
	fsetattrlistAddress uintptr
)

//go:cgo_import_dynamic windshare_output_futimens futimens "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic windshare_output_fsetattrlist fsetattrlist "/usr/lib/libSystem.B.dylib"
