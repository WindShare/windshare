//go:build linux

package mutationdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func linuxTreeSHA256WithBudget(rootFD int, budget *mutationTraversalBudget) (string, error) {
	records, err := walkLinuxTree(rootFD, "", "", budget, 0)
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	return hashBytes([]byte(strings.Join(records, "\n"))), nil
}

func copyLinuxTreeWithBudget(rootFD int, destination string, budget *mutationTraversalBudget) (string, error) {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	records, err := walkLinuxTree(rootFD, "", destination, budget, 0)
	if err != nil {
		return "", err
	}
	sort.Strings(records)
	return hashBytes([]byte(strings.Join(records, "\n"))), nil
}

func walkLinuxTree(
	directoryFD int,
	prefix string,
	destination string,
	budget *mutationTraversalBudget,
	depth int,
) (records []string, resultErr error) {
	// Reopening through the retained directory handle creates an independent
	// directory cursor. dup(2) would share the cursor and make the first identity
	// pass silently consume the entries needed by the later immutable copy.
	duplicate, err := unix.Openat(
		directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(duplicate), prefix)
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	seen := make(map[string]struct{})
	for {
		entries, readErr := directory.ReadDir(mutationTreeBatchSize)
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
				return nil, fmt.Errorf("private mutation input contains invalid name %q", name)
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("private mutation input enumeration repeated name %q", name)
			}
			seen[name] = struct{}{}
			relative := filepath.ToSlash(filepath.Join(prefix, name))
			var observed unix.Stat_t
			if err := unix.Fstatat(directoryFD, name, &observed, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return nil, err
			}
			switch observed.Mode & unix.S_IFMT {
			case unix.S_IFDIR:
				if err := budget.admitObject(relative, depth+1, 0); err != nil {
					return nil, err
				}
				childFD, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if err != nil {
					return nil, err
				}
				var stable unix.Stat_t
				if err := unix.Fstat(childFD, &stable); err != nil || stable.Dev != observed.Dev || stable.Ino != observed.Ino ||
					stable.Mode&unix.S_IFMT != unix.S_IFDIR {
					return nil, errors.Join(errors.New("private mutation input directory changed during retained open"), err, unix.Close(childFD))
				}
				childDestination := ""
				if destination != "" {
					childDestination = filepath.Join(destination, name)
					expectedMode := uint32(observed.Mode & 0o777)
					if err := os.Mkdir(childDestination, 0o700); err != nil {
						_ = unix.Close(childFD)
						return nil, err
					}
					destinationFD, err := unix.Open(
						childDestination, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
					)
					if err != nil {
						_ = unix.Close(childFD)
						return nil, err
					}
					var copied unix.Stat_t
					modeErr := unix.Fchmod(destinationFD, expectedMode)
					statErr := unix.Fstat(destinationFD, &copied)
					closeErr := unix.Close(destinationFD)
					if err := errors.Join(modeErr, statErr, closeErr); err != nil || copied.Mode&0o777 != expectedMode {
						_ = unix.Close(childFD)
						return nil, errors.Join(fmt.Errorf("copied directory %s did not retain its admitted mode", relative), err)
					}
				}
				childRecords, walkErr := walkLinuxTree(childFD, relative, childDestination, budget, depth+1)
				closeErr := unix.Close(childFD)
				if err := errors.Join(walkErr, closeErr); err != nil {
					return nil, err
				}
				records = append(records, fmt.Sprintf("D\x00%s\x00%o", relative, observed.Mode&0o777))
				records = append(records, childRecords...)
			case unix.S_IFREG:
				if observed.Nlink != 1 {
					return nil, fmt.Errorf("private mutation input %s is not a single-link regular file", relative)
				}
				if err := budget.admitObject(relative, depth+1, observed.Size); err != nil {
					return nil, err
				}
				fd, err := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				if err != nil {
					return nil, err
				}
				var stable unix.Stat_t
				if err := unix.Fstat(fd, &stable); err != nil || stable.Dev != observed.Dev || stable.Ino != observed.Ino ||
					stable.Mode&unix.S_IFMT != unix.S_IFREG || stable.Nlink != 1 || stable.Size != observed.Size {
					return nil, errors.Join(errors.New("private mutation input changed during retained open"), err, unix.Close(fd))
				}
				file := os.NewFile(uintptr(fd), relative)
				hasher := sha256.New()
				writer := io.Writer(hasher)
				var output *os.File
				if destination != "" {
					output, err = os.OpenFile(
						filepath.Join(destination, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL,
						os.FileMode(stable.Mode&0o777),
					)
					if err != nil {
						return nil, errors.Join(err, file.Close())
					}
					writer = io.MultiWriter(hasher, output)
				}
				written, copyErr := io.CopyN(writer, file, stable.Size)
				var extra [1]byte
				extraBytes, extraErr := file.Read(extra[:])
				if errors.Is(extraErr, io.EOF) {
					extraErr = nil
				}
				var outputErr error
				if output != nil {
					expectedMode := os.FileMode(stable.Mode & 0o777)
					modeErr := output.Chmod(expectedMode)
					copied, statErr := output.Stat()
					if statErr == nil && copied.Mode().Perm() != expectedMode {
						statErr = fmt.Errorf("copied file %s did not retain its admitted mode", relative)
					}
					outputErr = errors.Join(modeErr, statErr, output.Sync(), output.Close())
				}
				if err := errors.Join(copyErr, extraErr, outputErr, file.Close()); err != nil ||
					written != stable.Size || extraBytes != 0 {
					return nil, errors.Join(fmt.Errorf("retained input %s changed while copying", relative), err)
				}
				records = append(records, fmt.Sprintf(
					"F\x00%s\x00%d\x00%o\x00%s", relative, stable.Size, stable.Mode&0o777,
					hex.EncodeToString(hasher.Sum(nil)),
				))
			default:
				if err := budget.admitObject(relative, depth+1, 0); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("private mutation input contains unsupported object %s", relative)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}
