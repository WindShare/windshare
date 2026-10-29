//go:build darwin

package outputportable

import (
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

// x/sys exposes only microsecond futimes on Darwin. Use its libSystem calling
// convention to preserve nanoseconds and file identity without requiring CGO.
//
//go:linkname timestampLibSystemCall6 syscall.syscall6
//go:uintptrescapes
func timestampLibSystemCall6(function, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, errno syscall.Errno)

var futimensAddress uintptr

//go:cgo_import_dynamic windshare_output_futimens futimens "/usr/lib/libSystem.B.dylib"
