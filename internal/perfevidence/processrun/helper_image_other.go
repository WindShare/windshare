//go:build !linux && !windows

package processrun

import "errors"

func exactHelperPath() (string, error) {
	return "", errors.New("process ownership is unsupported on this platform")
}
