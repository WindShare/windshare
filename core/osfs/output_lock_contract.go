package osfs

import "errors"

var errKernelOutputLockUnavailable = errors.New("osfs: kernel-backed output locking is unavailable")

func unsupportedOutputLockError() error {
	return errors.Join(ErrUnsupportedOutputVolume, errKernelOutputLockUnavailable)
}
