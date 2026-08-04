//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package liveshare

import (
	"errors"
	"os"
)

// Process-scoped record locks are not a safe fallback: a registry sweep in the
// same process could acquire an already-owned file and delete its live root.
var errCatalogFileLockUnsupported = errors.New("live catalog storage requires descriptor-scoped file locking")

func lockCatalogFile(*os.File, bool) error {
	return errCatalogFileLockUnsupported
}

func tryLockCatalogFile(*os.File) (bool, error) {
	return false, errCatalogFileLockUnsupported
}

func unlockAndCloseCatalogFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
