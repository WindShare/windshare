//go:build linux

package osfs

import "os"

func v3RecoveryMakePrivateEnvelopeUnsafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0o755)
	}
	return os.Chmod(path, 0o644)
}
