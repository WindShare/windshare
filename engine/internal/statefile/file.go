// Package statefile owns private atomic files and recoverable local commits.
package statefile

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const (
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
	lockRetry            = 10 * time.Millisecond
	stagingPrefix        = ".state-"
)

func Write(path string, data []byte) (err error) {
	if path == "" {
		return os.ErrInvalid
	}
	if info, failure := os.Lstat(path); failure == nil && !info.Mode().IsRegular() {
		return errors.New("state destination must be a regular file")
	} else if failure != nil && !errors.Is(failure, os.ErrNotExist) {
		return failure
	}
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	staging := filepath.Join(filepath.Dir(path), stagingPrefix+rand.Text())
	file, err := createPrivateFile(staging)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		if failure := os.Remove(staging); failure != nil && !errors.Is(failure, os.ErrNotExist) {
			err = errors.Join(err, failure)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return replaceFile(staging, path)
}

func Remove(path string) error {
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return syncDirectory(path)
}

func WithLock(ctx context.Context, path string, action func() error) error {
	if path == "" {
		return os.ErrInvalid
	}
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	// A stable inode keeps waiters on the same lock across process boundaries.
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryConfigurationLock(file)
		if err != nil {
			return err
		}
		if locked {
			defer releaseConfigurationLock(file)
			return action()
		}
		timer := time.NewTimer(lockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Persist a newly created namespace before publishing a recovery record inside it.
func ensureDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return errors.New("state parent is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return os.ErrInvalid
	}
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, privateDirectoryMode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDirectory(path)
}
