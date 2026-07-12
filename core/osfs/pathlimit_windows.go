package osfs

import (
	"errors"
	"syscall"
	"unicode/utf16"
)

// windowsMaxPath = Win32 MAX_PATH(260),以 UTF-16 码元计且含结尾 NUL。
// M1 不启用 \\?\ 长路径前缀:即便 Go 自身能写长路径,产物也会让资源管理器
// 等常规工具无法访问,故超限明确报错(§6.13)。
const windowsMaxPath = 260

// ERROR_FILENAME_EXCED_RANGE is the Win32 error returned by APIs that reject a
// path or component before Go can translate it to syscall.ENAMETOOLONG.
const windowsErrorFilenameExceedsRange syscall.Errno = 206

func exceedsPathLimit(abs string) bool {
	// Win32 以 UTF-16 码元计长,Go 字符串按 UTF-8 字节计,须转码后再比;
	// +1 计入结尾 NUL。
	return len(utf16.Encode([]rune(abs)))+1 > windowsMaxPath
}

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, windowsErrorFilenameExceedsRange)
}
