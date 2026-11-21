//go:build !unix

package main

import "os"

func openRegularNoFollow(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errNonRegularFile
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		_ = file.Close()
		return nil, errNonRegularFile
	}
	return file, nil
}

// Non-Unix targets (including Windows) cannot fsync a directory handle opened via os.Open.
func syncDirectory(string) error { return nil }
