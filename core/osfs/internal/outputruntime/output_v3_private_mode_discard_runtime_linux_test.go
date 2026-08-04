//go:build linux

package outputruntime

import "os"

func runtimeMakePrivateEnvelopeUnsafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	markPortableRuntimePrivateEnvelopeUnsafe(path)
	return nil
}
