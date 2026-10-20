//go:build !windows

package relayconfig

import "os"

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
