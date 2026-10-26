//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/unix"
)

type linuxOutputProbeRootLock struct {
	directory *linuxOutputDirectory
}

func (root *linuxOutputDirectory) acquireOutputProbeLock() (*linuxOutputProbeRootLock, error) {
	const operation = "lock output feature probe"
	directory, err := root.Duplicate()
	if err != nil {
		return nil, err
	}
	// Each reopen has an independent open-file description, so flock also
	// serializes concurrent calls made through the same root handle. Locking the
	// fixed root itself avoids a removable lock-name ABA window before control
	// state exists.
	if err := directory.system.flock(directory.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := directory.close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(errLinuxOutputLockBusy, err, closeErr)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, linuxUnsupported(operation, "root-bound feature-probe locking is unavailable", errors.Join(err, closeErr))
		}
		return nil, errors.Join(fmt.Errorf("%s: %w", operation, err), closeErr)
	}
	same, err := linuxSameOpenDirectory(root, directory)
	if err != nil || !same {
		unlockErr := directory.system.flock(directory.fd, unix.LOCK_UN)
		return nil, errors.Join(
			linuxUnsafe(operation, "locked directory differs from the fixed output root", nil),
			err,
			unlockErr,
			directory.close(),
		)
	}
	return &linuxOutputProbeRootLock{directory: directory}, nil
}

func (root *linuxOutputDirectory) releaseOutputProbeLock(lock *linuxOutputProbeRootLock) error {
	if lock == nil || lock.directory == nil || lock.directory.fd < 0 {
		return linuxUnsafe("release output feature probe", "probe lock authority is absent", nil)
	}
	same, compareErr := linuxSameOpenDirectory(root, lock.directory)
	if compareErr != nil || !same {
		return errors.Join(
			linuxUnsafe("release output feature probe", "locked directory differs from the fixed output root", nil),
			compareErr,
			lock.directory.close(),
		)
	}
	unlockErr := lock.directory.system.flock(lock.directory.fd, unix.LOCK_UN)
	closeErr := lock.directory.close()
	lock.directory = nil
	return errors.Join(unlockErr, closeErr)
}

func (root *linuxOutputDirectory) recoverOutputProbeLeftovers() error {
	names, err := root.namesWithPrefix(outputprobe.ReservedPrefix, outputprobe.MaximumLeftovers+1)
	if err != nil {
		return err
	}
	return linuxProbeError(outputprobe.Recover(&linuxDirectory{native: root}, names, outputprobe.LinuxExt4))
}
