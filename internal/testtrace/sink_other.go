//go:build !linux && !windows

package testtrace

import (
	"errors"
	"os"
)

func openEventFile() (*os.File, error) {
	return nil, errors.New("private test-event transport is unsupported on this platform")
}
