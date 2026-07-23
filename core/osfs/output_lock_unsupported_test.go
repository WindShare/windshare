//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package osfs

import (
	"errors"
	"testing"
)

func TestUnsupportedPlatformOutputLockFailsClosed(t *testing.T) {
	for name, operation := range map[string]func() error{
		"lock":   func() error { return lockOutputFile(nil) },
		"unlock": func() error { return unlockOutputFile(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, ErrUnsupportedOutputVolume) {
				t.Fatalf("unsupported output %s error = %v", name, err)
			}
		})
	}
}
