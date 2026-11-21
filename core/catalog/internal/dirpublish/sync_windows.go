package dirpublish

// SyncDir is a no-op on Windows: unprivileged handles cannot flush a directory
// barrier (FlushFileBuffers fails with ERROR_ACCESS_DENIED), and opening the
// directory immediately before Rename only churns handles beneath it. Flushed
// files and atomic rename provide process-restart durability.
func SyncDir(string) error {
	return nil
}
