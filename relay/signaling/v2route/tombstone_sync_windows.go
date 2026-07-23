//go:build windows

package v2route

// Windows does not expose directory FlushFileBuffers through os.File. The
// tombstone file itself is created before startup and every acknowledged append
// is flushed through its file handle, so no rename/directory transaction is
// required on this platform.
func syncDirectory(string) error { return nil }
