//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"io/fs"
)

func (directory *linuxOutputDirectory) rollbackCreatedFile(name string, created *linuxOutputRegularFile) error {
	// Exclusive creation already owns the open object, even if its initial
	// inspection or permission setup failed. Compare live handles without
	// requiring the unfinished file to satisfy its intended mutation policy.
	current, err := directory.openRegularFile(name, linuxOutputFileObserved)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	same, compareErr := linuxSameOpenRegularFile(created, current)
	closeErr := current.close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(
			linuxUnsafe("roll back created output file", "name no longer proves the created object's identity", nil),
			compareErr, closeErr,
		)
	}
	if err := directory.system.unlinkat(directory.fd, name, 0); err != nil {
		return fmt.Errorf("roll back created output file %q: %w", name, err)
	}
	return directory.sync()
}
