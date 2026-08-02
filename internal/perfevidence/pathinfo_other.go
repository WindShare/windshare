//go:build !windows

package perfevidence

import "os"

// isReparsePointInfo is the no-follow boundary used by the evidence store.
// Unix filesystems expose symbolic links through ModeSymlink; there is no
// Windows-style reparse attribute to inspect on these targets.
func isReparsePointInfo(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}
