//go:build linux

package perfevidence

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func (stage *stageDirectoryAuthority) walkEvidenceStore(walk *evidenceStoreWalk) error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	return walkLinuxRegularFiles(stage.fd, "", walk, stage.transition)
}

func walkLinuxRegularFiles(
	fd int,
	relative string,
	walk *evidenceStoreWalk,
	transition func(string, string) error,
) error {
	entries, err := readLinuxDirectory(fd, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		var before unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := walk.observeDirectory(childRelative); err != nil {
				return err
			}
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			walkErr := requireLinuxHandleIdentity(child, before)
			if walkErr == nil && transition != nil {
				walkErr = transition(childRelative, "directory-opened")
			}
			if walkErr == nil {
				walkErr = walkLinuxRegularFiles(child, childRelative, walk, transition)
			}
			closeErr := unix.Close(child)
			if err := errors.Join(walkErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		case unix.S_IFREG:
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			file := os.NewFile(uintptr(child), childRelative)
			info, statErr := file.Stat()
			visitErr := statErr
			if visitErr == nil {
				visitErr = requireLinuxHandleIdentity(child, before)
			}
			if visitErr == nil {
				if transition != nil {
					visitErr = transition(childRelative, "file-opened")
				}
			}
			if visitErr == nil {
				visitErr = walk.observeFile(childRelative, file, info)
			}
			closeErr := file.Close()
			if err := errors.Join(visitErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("artifact %s is a symlink or unsupported filesystem object", childRelative)
		}
	}
	return nil
}

func (stage *stageDirectoryAuthority) syncContents() error {
	if stage == nil || stage.fd < 0 {
		return errors.New("stage directory authority is closed")
	}
	return syncLinuxDirectoryContents(stage.fd, "", stage.transition, defaultEvidenceStoreMeter())
}

func syncLinuxDirectoryContents(
	fd int,
	relative string,
	transition func(string, string) error,
	meter *evidenceStoreMeter,
) error {
	entries, err := readLinuxDirectory(fd, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		var before unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		switch before.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := meter.observeDirectory(childRelative, evidenceRelativeDepth(childRelative)); err != nil {
				return err
			}
			child, err := openLinuxChild(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
			if err != nil {
				return err
			}
			syncErr := requireLinuxHandleIdentity(child, before)
			if syncErr == nil && transition != nil {
				syncErr = transition(childRelative, "directory-opened")
			}
			if syncErr == nil {
				syncErr = syncLinuxDirectoryContents(child, childRelative, transition, meter)
			}
			closeErr := unix.Close(child)
			if err := errors.Join(syncErr, closeErr, verifyLinuxEntryIdentity(fd, entry.Name(), before)); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := meter.observeFile(
				childRelative, evidenceRelativeDepth(childRelative), before.Size, evidenceArtifactFile,
			); err != nil {
				return err
			}
			if transition != nil {
				if err := transition(childRelative, "file-opened"); err != nil {
					return err
				}
			}
			if err := syncLinuxRegularFile(fd, entry.Name(), before); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing to sync symlink or unsupported artifact %s", childRelative)
		}
	}
	return unix.Fsync(fd)
}

func syncLinuxRegularFile(parent int, name string, before unix.Stat_t) error {
	readFD, err := openLinuxChild(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if err != nil {
		return err
	}
	if err := requireLinuxHandleIdentity(readFD, before); err != nil {
		return errors.Join(err, unix.Close(readFD))
	}
	originalMode := before.Mode & 0o7777
	if err := unix.Fchmod(readFD, originalMode|0o200); err != nil {
		return errors.Join(err, unix.Close(readFD))
	}
	writeFD, openErr := openLinuxChild(parent, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW)
	if openErr == nil {
		openErr = requireLinuxHandleIdentity(writeFD, before)
	}
	var syncErr, writeCloseErr error
	if openErr == nil {
		syncErr = unix.Fsync(writeFD)
	}
	if writeFD >= 0 {
		writeCloseErr = unix.Close(writeFD)
	}
	restoreErr := unix.Fchmod(readFD, originalMode)
	readCloseErr := unix.Close(readFD)
	return errors.Join(
		openErr, syncErr, writeCloseErr, restoreErr, readCloseErr,
		verifyLinuxEntryIdentity(parent, name, before),
	)
}

func openLinuxChild(parent int, name string, flags int) (int, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	}
	return unix.Openat2(parent, name, &how)
}

func authorityChildAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT)
}

func requireLinuxHandleIdentity(fd int, expected unix.Stat_t) error {
	var observed unix.Stat_t
	if err := unix.Fstat(fd, &observed); err != nil {
		return err
	}
	if observed.Dev != expected.Dev || observed.Ino != expected.Ino || observed.Mode&unix.S_IFMT != expected.Mode&unix.S_IFMT {
		return errors.New("filesystem entry changed between no-follow inspection and open")
	}
	return nil
}

func verifyLinuxEntryIdentity(parent int, name string, expected unix.Stat_t) error {
	var observed unix.Stat_t
	if err := unix.Fstatat(parent, name, &observed, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if observed.Dev != expected.Dev || observed.Ino != expected.Ino || observed.Mode&unix.S_IFMT != expected.Mode&unix.S_IFMT {
		return errors.New("filesystem entry changed during handle-relative traversal")
	}
	return nil
}

func (authority *outputRootAuthority) readDir() ([]os.DirEntry, error) {
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	entries, err := readLinuxDirectory(authority.fd, authority.path)
	if err != nil {
		return nil, err
	}
	meter := defaultEvidenceStoreMeter()
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return nil, err
	}
	return entries, nil
}

func (authority *outputRootAuthority) removeChild(name string, transition func(string) error) error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	return removeLinuxEntryAt(authority.fd, name, name, transition)
}

func removeLinuxEntryAt(parent int, name, relative string, transition func(string) error) error {
	for range cleanupMutationLimit {
		var stat unix.Stat_t
		err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Unlinkat(parent, name, 0); errors.Is(err, unix.ENOENT) {
				return nil
			} else if err != nil {
				continue
			}
			return nil
		}
		child, err := openLinuxChild(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			continue
		}
		if err != nil {
			return err
		}
		if transition != nil {
			if err := transition(relative); err != nil {
				return errors.Join(err, unix.Close(child))
			}
		}
		emptyErr := emptyLinuxDirectory(child, relative, transition)
		closeErr := unix.Close(child)
		if err := errors.Join(emptyErr, closeErr); err != nil {
			return err
		}
		err = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.ENOTDIR) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return fmt.Errorf("directory entry %s kept changing during handle-relative cleanup", relative)
}

func emptyLinuxDirectory(fd int, relative string, transition func(string) error) error {
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return err
	}
	for range cleanupMutationLimit {
		entries, err := readLinuxDirectory(fd, relative)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
			if err := removeLinuxEntryAt(fd, entry.Name(), child, transition); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("directory %s did not become empty during handle-relative cleanup", relative)
}

func readLinuxDirectory(fd int, name string) ([]os.DirEntry, error) {
	duplicate, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), name)
	maximumEntries := DefaultEvidenceStoreBudget().MaxObjects
	var entries []os.DirEntry
	var readErr error
	for {
		batch, err := file.ReadDir(evidenceStoreReadBatch)
		if len(batch) > maximumEntries-len(entries) {
			readErr = fmt.Errorf("evidence directory %s exceeds %d entries", name, maximumEntries)
			break
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	return entries, errors.Join(readErr, file.Close())
}

func platformPathKey(path string) string {
	return filepath.Clean(path)
}

func platformPathAlias(_, _ string) bool {
	return false
}

func physicalMemory() (bytes uint64, probe string, resultErr error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, "/proc/meminfo", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "MemTotal:" || fields[2] != "kB" {
			continue
		}
		kilobytes, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || kilobytes == 0 {
			return 0, "/proc/meminfo", errors.New("MemTotal was invalid")
		}
		return kilobytes * 1024, "/proc/meminfo", nil
	}
	if err := scanner.Err(); err != nil {
		return 0, "/proc/meminfo", err
	}
	return 0, "/proc/meminfo", errors.New("MemTotal was absent")
}

func cpuModel() (model string, resultErr error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if found && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", scanner.Err()
}

func osDescription() string {
	encoded, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for line := range strings.SplitSeq(string(encoded), "\n") {
		if value, found := strings.CutPrefix(line, "PRETTY_NAME="); found {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return "Linux"
}

func currentProcessToken() (string, error) {
	token, _, err := linuxProcessToken(os.Getpid())
	return token, err
}

func processMatches(processID int, token string) (bool, error) {
	if processID <= 0 {
		return false, nil
	}
	observed, state, err := linuxProcessToken(processID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state == "Z" {
		return false, nil
	}
	if err := unix.Kill(processID, 0); errors.Is(err, unix.ESRCH) {
		return false, nil
	} else if err != nil && !errors.Is(err, unix.EPERM) {
		return false, err
	}
	return observed == token, nil
}

func linuxProcessToken(processID int) (string, string, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(processID), "stat"))
	if err != nil {
		return "", "", err
	}
	closing := strings.LastIndexByte(string(stat), ')')
	if closing < 0 {
		return "", "", errors.New("process stat omitted command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndexAfterCommand = 19
	if len(fields) <= startTimeIndexAfterCommand {
		return "", "", errors.New("process stat omitted start time")
	}
	state := fields[0]
	token := fmt.Sprintf("%s:%s", strings.TrimSpace(string(bootID)), fields[startTimeIndexAfterCommand])
	return token, state, nil
}

func (authority *outputRootAuthority) sync() error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	return unix.Fsync(authority.fd)
}

func (authority *outputRootAuthority) renameChildNoReplace(
	stage *stageDirectoryAuthority,
	destination string,
) error {
	if authority == nil || authority.fd < 0 {
		return errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(destination); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if stage.transition != nil {
		if err := stage.transition("", "rename-source-verified"); err != nil {
			return err
		}
	}
	return unix.Renameat2(authority.fd, stage.name, authority.fd, destination, unix.RENAME_NOREPLACE)
}
