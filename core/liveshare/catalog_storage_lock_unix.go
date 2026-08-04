//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package liveshare

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockCatalogFile(file *os.File, nonblocking bool) error {
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	return unix.Flock(int(file.Fd()), operation)
}

func tryLockCatalogFile(file *os.File) (bool, error) {
	err := lockCatalogFile(file, true)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockAndCloseCatalogFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
}
