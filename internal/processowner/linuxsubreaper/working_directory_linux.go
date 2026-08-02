//go:build linux

package linuxsubreaper

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type workingDirectoryAuthority struct {
	file *os.File
}

func holdWorkingDirectory(path string) (*workingDirectoryAuthority, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("owned working directory is not a no-follow directory")
	}
	descriptor, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = unix.Close(descriptor)
		return nil, errors.New("owned working-directory descriptor could not be adopted")
	}
	opened, err := file.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.Join(err, errors.New("owned working directory changed while opened"))
	}
	return &workingDirectoryAuthority{file: file}, nil
}

func authenticateHeldWorkingDirectory(file *os.File) error {
	if err := validateDirectoryPathDescriptor(int(file.Fd()), "exec-gate working directory"); err != nil {
		return err
	}
	metadata, err := file.Stat()
	if err != nil {
		return err
	}
	if !metadata.IsDir() {
		return errors.New("exec-gate working-directory capability is not a directory")
	}
	return nil
}

func (authority *workingDirectoryAuthority) close() {
	if authority != nil && authority.file != nil {
		_ = authority.file.Close()
		authority.file = nil
	}
}
