//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package osfs

import (
	"errors"
	"os"
	"syscall"
)

func lockOutputFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return errors.Join(ErrOutputSessionActive, err)
		}
		return err
	}
	return nil
}

func unlockOutputFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
