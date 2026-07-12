package osfs

import (
	"io/fs"
	"syscall"
)

// isReparsePoint(Windows):符号链接只是 reparse point 的一种;junction/挂载点
// 与 OneDrive 占位等在新版 Go 里不再映射为 ModeSymlink(常为 ModeIrregular),
// 只有直接看 FILE_ATTRIBUTE_REPARSE_POINT 属性位才不漏(§6.13"一切 reparse
// point 不跟随")。ModeSymlink 分支保留作双保险。
func isReparsePoint(fi fs.FileInfo) bool {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	sys, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && sys.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
