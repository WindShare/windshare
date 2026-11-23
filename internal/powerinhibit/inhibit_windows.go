//go:build windows

package powerinhibit

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// Win32 POWER_REQUEST_CONTEXT_VERSION and POWER_REQUEST_CONTEXT_SIMPLE_STRING.
	powerRequestContextVersion      = 0
	powerRequestContextSimpleString = 0x00000001
	// PowerRequestSystemRequired prevents idle system sleep while permitting
	// the display to turn off and lock normally.
	powerRequestSystemRequired = 1
)

// reasonContext mirrors Win32 REASON_CONTEXT. Trailing padding reserves the
// full size of the union's Detailed arm so PowerCreateRequest sees a complete
// struct across both 32-bit and 64-bit Windows ABIs.
type reasonContext struct {
	version      uint32
	flags        uint32
	simpleReason *uint16
	_            [3]uintptr
}

var (
	kernel32DLL     = windows.NewLazySystemDLL("kernel32.dll")
	procPowerCreate = kernel32DLL.NewProc("PowerCreateRequest")
	procPowerSet    = kernel32DLL.NewProc("PowerSetRequest")
	procPowerClear  = kernel32DLL.NewProc("PowerClearRequest")
)

// acquireSystemSleepInhibition uses handle-based PowerCreateRequest rather than
// SetThreadExecutionState because Go goroutines migrate across OS threads.
// The kernel automatically reclaims the handle if the process terminates.
func acquireSystemSleepInhibition(reason string) (func(), error) {
	reasonUTF16, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return nil, fmt.Errorf("encode sleep inhibit reason: %w", err)
	}
	context := reasonContext{
		version:      powerRequestContextVersion,
		flags:        powerRequestContextSimpleString,
		simpleReason: reasonUTF16,
	}
	r1, _, callErr := procPowerCreate.Call(uintptr(unsafe.Pointer(&context)))
	runtime.KeepAlive(reasonUTF16)
	handle := windows.Handle(r1)
	if handle == 0 || handle == windows.InvalidHandle {
		return nil, fmt.Errorf("PowerCreateRequest: %w", callErr)
	}
	ok, _, setErr := procPowerSet.Call(uintptr(handle), powerRequestSystemRequired)
	if ok == 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("PowerSetRequest: %w", setErr)
	}
	return func() {
		_, _, _ = procPowerClear.Call(uintptr(handle), powerRequestSystemRequired)
		_ = windows.CloseHandle(handle)
	}, nil
}
