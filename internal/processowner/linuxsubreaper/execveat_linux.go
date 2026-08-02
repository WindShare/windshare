//go:build linux

package linuxsubreaper

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// These are the same runtime gates used by syscall.Exec. Calling execveat
// without them would let the Go runtime create an OS thread while the kernel is
// replacing the process image.
//
//go:linkname runtimeBeforeExec syscall.runtime_BeforeExec
func runtimeBeforeExec()

//go:linkname runtimeAfterExec syscall.runtime_AfterExec
func runtimeAfterExec()

func execveat(descriptor int, arguments, environment []string) error {
	emptyPath, err := unix.BytePtrFromString("")
	if err != nil {
		return err
	}
	argumentPointers, err := syscall.SlicePtrFromStrings(arguments)
	if err != nil {
		return err
	}
	environmentPointers, err := syscall.SlicePtrFromStrings(environment)
	if err != nil {
		return err
	}
	runtimeBeforeExec()
	_, _, callErr := unix.RawSyscall6(
		unix.SYS_EXECVEAT,
		uintptr(descriptor),
		uintptr(unsafe.Pointer(emptyPath)),
		stringVectorPointer(argumentPointers),
		stringVectorPointer(environmentPointers),
		unix.AT_EMPTY_PATH,
		0,
	)
	runtimeAfterExec()
	if callErr != 0 {
		return callErr
	}
	return nil
}

func stringVectorPointer(vector []*byte) uintptr {
	if len(vector) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&vector[0]))
}
