//go:build linux

package outputportable

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const (
	procFileDescriptorDirectory = "/proc/self/fd"
	nativeSpellingBatchSize     = 128
)

func entrySpelling(parent *directory, name string) (spelling string, resultErr error) {
	// O_PATH observes the resolved dentry without reading foreign file data.
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	handle := os.NewFile(uintptr(fd), name)
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	path, err := os.Readlink(filepath.Join(procFileDescriptorDirectory, strconv.Itoa(fd)))
	if err == nil {
		return filepath.Base(path), nil
	}
	// Chroots need not mount procfs. Enumeration is only the fallback; normal
	// admission reads the already-open dentry without scanning siblings.
	info, statErr := handle.Stat()
	if statErr != nil {
		return "", statErr
	}
	directory, openErr := parent.root.Open(".")
	if openErr != nil {
		return "", openErr
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	var alias string
	for {
		entries, readErr := directory.ReadDir(nativeSpellingBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		for _, entry := range entries {
			candidate, statErr := entry.Info()
			if statErr != nil {
				return "", statErr
			}
			if os.SameFile(info, candidate) {
				if entry.Name() == name {
					return name, nil
				}
				alias = entry.Name()
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if alias != "" {
		return alias, nil
	}
	return "", outputcap.ErrUnsafeNamespace
}

func exactEntrySpelling(requested, actual string) bool { return requested == actual }

func validateNativeName(string) error { return nil }
