//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func installJournal(tempPath, targetPath string) error {
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}

	// Syncing only the file does not make its new directory entry durable on
	// filesystems that can lose a completed rename across power failure.
	parent, err := os.Open(filepath.Dir(targetPath))
	if err != nil {
		return fmt.Errorf("open parent directory for sync: %w", err)
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("sync parent directory: %w", errors.Join(syncErr, closeErr))
	}
	return nil
}
