//go:build windows

package statefile

// MoveFileEx already requests write-through publication on Windows.
func syncDirectory(string) error { return nil }
