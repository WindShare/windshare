//go:build !windows

package osfs

import "io/fs"

// isReparsePoint(POSIX):reparse point 是 NTFS 概念,POSIX 侧的对应物即符号
// 链接;Lstat 语义下 ModeSymlink 即足够(§6.13 不跟随、不打包)。
func isReparsePoint(fi fs.FileInfo) bool {
	return fi.Mode()&fs.ModeSymlink != 0
}
