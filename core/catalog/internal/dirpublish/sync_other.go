//go:build !windows

package dirpublish

import (
	"errors"
	"os"
)

func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
