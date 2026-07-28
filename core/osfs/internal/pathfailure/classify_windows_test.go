//go:build windows

package pathfailure

import (
	"errors"
	"syscall"
	"testing"
)

func TestWindowsPathLengthClassificationSurvivesWrapping(t *testing.T) {
	if !IsTooLong(errors.Join(errors.New("context"), syscall.ENAMETOOLONG)) ||
		!IsTooLong(windowsErrorFilenameExceedsRange) || IsTooLong(syscall.EINVAL) {
		t.Fatal("path-length error taxonomy is not stable under wrapping")
	}
}
