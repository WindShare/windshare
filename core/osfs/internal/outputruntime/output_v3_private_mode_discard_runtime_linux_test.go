//go:build linux

package outputruntime

import "os"

func runtimeMakePrivateEnvelopeUnsafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0o755)
	}
	return os.Chmod(path, 0o644)
}
