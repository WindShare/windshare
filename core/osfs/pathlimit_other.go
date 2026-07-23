//go:build !windows

package osfs

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"

	"github.com/windshare/windshare/core/transfer"
)

const (
	linuxMaxPath        = 4096
	portableUnixMaxPath = 1024
)

func isReparsePoint(information fs.FileInfo) bool {
	// Lstat reports symbolic links directly on POSIX, so this is the complete
	// no-follow boundary corresponding to Windows reparse-point rejection.
	return information.Mode()&fs.ModeSymlink != 0
}

func exceedsPathLimit(abs string) bool {
	// Linux and the BSD-derived platforms expose different PATH_MAX values.
	// Applying the smaller value everywhere would reject valid Linux outputs,
	// while applying Linux's value everywhere would create paths ordinary macOS
	// APIs cannot reopen.
	maxPath := portableUnixMaxPath
	if runtime.GOOS == "linux" {
		maxPath = linuxMaxPath
	}
	return len(abs)+1 > maxPath
}

func isPathTooLongError(err error) bool {
	return errors.Is(err, syscall.ENAMETOOLONG)
}

func isDirectoryNotEmptyError(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

func outputPlatformCapabilities() (transfer.DurabilityLevel, bool) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return transfer.DurabilityPowerLoss, true
	}
	return transfer.DurabilityProcessRestart, false
}

func lockOutputFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return errors.Join(ErrOutputSessionActive, err)
		}
		return err
	}
	return nil
}

func unlockOutputFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func outputObjectIdentity(file *os.File) (transfer.OutputObjectIdentity, error) {
	if file == nil {
		return transfer.OutputObjectIdentity{}, ErrOwnedFileMissing
	}
	info, err := file.Stat()
	if err != nil {
		return transfer.OutputObjectIdentity{}, err
	}
	value := reflect.Indirect(reflect.ValueOf(info.Sys()))
	device, deviceOK := outputIdentityField(value, "Dev")
	inode, inodeOK := outputIdentityField(value, "Ino")
	if !deviceOK || !inodeOK {
		return transfer.OutputObjectIdentity{}, errors.New("osfs: platform does not expose persistent output object identity")
	}
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], device)
	binary.BigEndian.PutUint64(raw[8:16], inode)
	digest := sha256.Sum256(append([]byte("windshare/output-object/posix/v1\x00"), raw[:]...))
	return transfer.OutputObjectIdentityFromBytes(digest[:])
}

func outputRootLocator(rootPath string, _ *os.File) (string, error) {
	return filepath.Clean(rootPath), nil
}

func validateOutputRootPlatform(*os.File) error { return nil }

func openOutputRemovalFile(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}

func removeOpenedOutputFile(root *os.Root, path string, _ *os.File) error {
	return root.Remove(path)
}

func outputIdentityField(value reflect.Value, name string) (uint64, bool) {
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0, false
	}
	field := value.FieldByName(name)
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() < 0 {
			return 0, false
		}
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}

func installOutputJournal(root *os.Root, tempName, targetName string) error {
	if err := root.Rename(tempName, targetName); err != nil {
		return err
	}
	return syncOutputDirectory(root, ".")
}

func publishOutputFile(root *os.Root, stage, target string) error {
	if err := root.Link(stage, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return categorizedPathFailure("publish output", target, ErrAlreadyExists, err)
		}
		return err
	}
	if err := syncOutputDirectory(root, filepath.Dir(target)); err != nil {
		return err
	}
	if err := root.Remove(stage); err != nil {
		return err
	}
	return syncOutputDirectory(root, ".")
}

func removeOutputJournal(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncOutputDirectory(root, ".")
}

func syncOutputDirectory(root *os.Root, relative string) error {
	directory, err := root.Open(relative)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
