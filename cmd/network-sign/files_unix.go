//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func openRegularNoFollow(path string) (*os.File, error) {
	// Refuse leaf symlinks and avoid blocking on FIFOs or device nodes before verifying file type.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errNonRegularFile
	}
	return file, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
