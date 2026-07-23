//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package osfs

import "os"

func lockOutputFile(*os.File) error {
	// A weaker process-scoped or userspace lock would allow two sessions in one
	// process to own the same journal. Refuse the backend when the platform
	// cannot provide the file-scoped lifetime contract.
	return unsupportedOutputLockError()
}

func unlockOutputFile(*os.File) error {
	return unsupportedOutputLockError()
}
