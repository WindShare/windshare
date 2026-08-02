//go:build linux

package processrun

import (
	"errors"
	"fmt"
	"os"
)

const linuxSelfExecutable = "/proc/self/exe"

func exactHelperPath() (string, error) {
	info, err := os.Stat(linuxSelfExecutable)
	if err != nil {
		return "", fmt.Errorf("inspect retained process-owner helper image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("retained process-owner helper image is not a regular file")
	}
	// /proc/self/exe is a kernel-held reference to this exact image, so a pathname
	// replacement cannot redirect the privileged owner launch between validation
	// and exec.
	return linuxSelfExecutable, nil
}
