//go:build darwin

package outputdarwin

import (
	"errors"
	"os"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

type lock struct{ state *fileState }

func (held *lock) File() outputcap.MutableFile {
	if held == nil || held.state == nil {
		return nil
	}
	return &mutableFile{State: held.state}
}
func (held *lock) Close() error {
	if held == nil || held.state == nil {
		return nil
	}
	state := held.state
	held.state = nil
	state.borrowed = false
	// Closing the final descriptor releases flock even when explicit unlock
	// fails; the namespace entry remains the stable lock identity.
	return errors.Join(unix.Flock(int(state.file.Fd()), unix.LOCK_UN), state.Close())
}
func (dir *directory) AcquireLock(name string, existingOnly bool) (outputcap.Lock, bool, error) {
	state, err := dir.openFile(name, true, unix.O_RDWR)
	created := false
	if errors.Is(err, os.ErrNotExist) && !existingOnly {
		state, err = dir.openFile(name, true, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
		created = err == nil
		if errors.Is(err, outputcap.ErrNamespaceCollision) {
			state, err = dir.openFile(name, true, unix.O_RDWR)
		}
	}
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(state.file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := state.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, created, errors.Join(outputcap.ErrNamespaceLockBusy, closeErr)
		}
		return nil, created, errors.Join(nativeError(err), closeErr)
	}
	if err := state.verifyOrigin(); err != nil {
		return nil, created, errors.Join(err, state.Close())
	}
	state.borrowed = true
	return &lock{state: state}, created, nil
}
