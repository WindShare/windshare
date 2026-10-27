//go:build darwin

package outputdarwin

import (
	"syscall"
	_ "unsafe"
)

// These descriptor APIs are absent from x/sys/unix. Use the same runtime-to-
// libSystem bridge as x/sys so Darwin's userspace ABI owns syscall translation,
// without requiring CGO or weakening handle-relative identity checks.
//
//go:linkname libSystemCall6 syscall.syscall6
//go:uintptrescapes
func libSystemCall6(function, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, errno syscall.Errno)

var fgetattrlistAddress uintptr
var fsetattrlistAddress uintptr

//go:cgo_import_dynamic windshare_fgetattrlist fgetattrlist "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic windshare_fsetattrlist fsetattrlist "/usr/lib/libSystem.B.dylib"
