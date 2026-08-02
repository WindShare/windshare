//go:build windows

package processrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func exactHelperPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve loaded process-owner helper image: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve loaded process-owner helper path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect loaded process-owner helper image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("loaded process-owner helper image is not a regular file")
	}
	// Windows denies replacement of the currently mapped executable image; the
	// owner launch therefore names the same image whose path was resolved here.
	return absolute, nil
}
