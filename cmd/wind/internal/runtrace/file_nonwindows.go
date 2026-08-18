//go:build !windows

package runtrace

import "os"

func createOwnerOnlyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, ownerOnlyFileMode)
}
