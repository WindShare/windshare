//go:build linux

package osfs

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	linuxOutputProbeReservedPrefix   = ".windshare-output.probe"
	linuxOutputProbeMaximumLeftovers = 64
	linuxOutputProbeMaximumEntries   = 7
)

var linuxOutputProbeRegularNames = map[string]struct{}{
	"stage": {}, "anchor": {}, "publication": {}, "record": {}, "record.tmp": {},
}

var linuxOutputProbeDirectoryNames = map[string]struct{}{
	"candidate": {}, "installed": {},
}

type linuxOutputProbeRootLock struct {
	directory *linuxOutputDirectory
}

func (root *linuxOutputDirectory) acquireOutputProbeLock() (*linuxOutputProbeRootLock, error) {
	const operation = "lock output feature probe"
	directory, err := root.Duplicate()
	if err != nil {
		return nil, err
	}
	// Each reopen has an independent open-file description, so flock also
	// serializes concurrent calls made through the same root handle. Locking the
	// fixed root itself avoids a removable lock-name ABA window before control
	// state exists.
	if err := directory.system.flock(directory.fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := directory.close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(errLinuxOutputLockBusy, err, closeErr)
		}
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, linuxUnsupported(operation, "root-bound feature-probe locking is unavailable", errors.Join(err, closeErr))
		}
		return nil, errors.Join(fmt.Errorf("%s: %w", operation, err), closeErr)
	}
	same, err := linuxSameOpenDirectory(root, directory)
	if err != nil || !same {
		unlockErr := directory.system.flock(directory.fd, unix.LOCK_UN)
		return nil, errors.Join(
			linuxUnsafe(operation, "locked directory differs from the fixed output root", nil),
			err,
			unlockErr,
			directory.close(),
		)
	}
	return &linuxOutputProbeRootLock{directory: directory}, nil
}

func (root *linuxOutputDirectory) releaseOutputProbeLock(lock *linuxOutputProbeRootLock) error {
	if lock == nil || lock.directory == nil || lock.directory.fd < 0 {
		return linuxUnsafe("release output feature probe", "probe lock authority is absent", nil)
	}
	same, compareErr := linuxSameOpenDirectory(root, lock.directory)
	if compareErr != nil || !same {
		return errors.Join(
			linuxUnsafe("release output feature probe", "locked directory differs from the fixed output root", nil),
			compareErr,
			lock.directory.close(),
		)
	}
	unlockErr := lock.directory.system.flock(lock.directory.fd, unix.LOCK_UN)
	closeErr := lock.directory.close()
	lock.directory = nil
	return errors.Join(unlockErr, closeErr)
}

func (root *linuxOutputDirectory) recoverOutputProbeLeftovers() error {
	const operation = "recover Linux output feature probe"
	names, err := root.namesWithPrefix(linuxOutputProbeReservedPrefix, linuxOutputProbeMaximumLeftovers+1)
	if err != nil {
		return err
	}
	leftovers := make([]*linuxOutputProbeLeftover, 0, len(names))
	closeAll := func() error {
		var closeErr error
		for _, leftover := range leftovers {
			closeErr = errors.Join(closeErr, leftover.close())
		}
		return closeErr
	}
	for _, name := range names {
		if !linuxCanonicalProbeName(name) {
			return errors.Join(linuxUnsafe(operation, "malformed reserved probe name blocks the output root", nil), closeAll())
		}
		if len(leftovers) == linuxOutputProbeMaximumLeftovers {
			return errors.Join(linuxUnsafe(operation, "probe leftover count exceeds its safety bound", nil), closeAll())
		}
		leftover, inspectErr := root.inspectOutputProbeLeftover(name)
		if inspectErr != nil {
			return errors.Join(linuxUnsafe(operation, "probe leftover does not match the strict temporary schema", inspectErr), closeAll())
		}
		leftovers = append(leftovers, leftover)
	}
	for _, leftover := range leftovers {
		if err := leftover.remove(); err != nil {
			return errors.Join(linuxUnsafe(operation, "fixed probe leftover could not be reduced safely", err), closeAll())
		}
	}
	return closeAll()
}

func linuxCanonicalProbeName(name string) bool {
	if !strings.HasPrefix(name, linuxOutputProbePrefix) || len(name) != len(linuxOutputProbePrefix)+linuxOutputProbeRandomBytes*2 {
		return false
	}
	for _, character := range name[len(linuxOutputProbePrefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type linuxOutputProbeLeftover struct {
	root        *linuxOutputDirectory
	name        string
	directory   *linuxOutputDirectory
	regular     map[string]*linuxOutputRegularFile
	directories map[string]*linuxOutputDirectory
}

func (root *linuxOutputDirectory) inspectOutputProbeLeftover(name string) (*linuxOutputProbeLeftover, error) {
	directory, err := root.openDirectoryExact(name, linuxOutputDirectoryMode)
	if err != nil {
		return nil, err
	}
	leftover := &linuxOutputProbeLeftover{
		root: root, name: name, directory: directory,
		regular: make(map[string]*linuxOutputRegularFile), directories: make(map[string]*linuxOutputDirectory),
	}
	var observation outputV3ProbeCutObservation
	fail := func(cause error) (*linuxOutputProbeLeftover, error) {
		return nil, errors.Join(cause, leftover.close())
	}
	names, err := directory.names(linuxOutputProbeMaximumEntries)
	if err != nil {
		return fail(err)
	}
	for _, entry := range names {
		if _, ok := linuxOutputProbeRegularNames[entry]; ok {
			file, openErr := directory.openRegularFileExact(entry, false, linuxOutputStateFileMode)
			if openErr != nil {
				return fail(openErr)
			}
			identity, identityErr := file.currentIdentity()
			if identityErr != nil {
				return fail(errors.Join(fmt.Errorf("inspect probe file %q", entry), identityErr, file.close()))
			}
			if observeErr := observation.observeFile(entry, identity.size); observeErr != nil {
				return fail(errors.Join(observeErr, file.close()))
			}
			leftover.regular[entry] = file
			continue
		}
		if _, ok := linuxOutputProbeDirectoryNames[entry]; ok {
			child, openErr := directory.openDirectoryExact(entry, linuxOutputDirectoryMode)
			if openErr != nil {
				return fail(openErr)
			}
			childNames, enumerateErr := child.names(0)
			if enumerateErr != nil || len(childNames) != 0 {
				return fail(errors.Join(fmt.Errorf("probe directory %q is not empty", entry), enumerateErr, child.close()))
			}
			if observeErr := observation.observeDirectory(entry); observeErr != nil {
				return fail(errors.Join(observeErr, child.close()))
			}
			leftover.directories[entry] = child
			continue
		}
		return fail(fmt.Errorf("unexpected probe entry %q", entry))
	}
	if err := leftover.validateDataLinks(); err != nil {
		return fail(err)
	}
	if err := validateOutputV3ProbeCut(outputV3ProbeDataLinuxExt4, observation); err != nil {
		return fail(err)
	}
	return leftover, nil
}

func (leftover *linuxOutputProbeLeftover) validateDataLinks() error {
	var witness *linuxOutputRegularFile
	for _, name := range []string{"stage", "anchor", "publication"} {
		file := leftover.regular[name]
		if file == nil {
			continue
		}
		if witness == nil {
			witness = file
			continue
		}
		same, err := linuxSameOpenRegularFile(witness, file)
		if err != nil || !same {
			return errors.Join(errors.New("probe stage, anchor, and publication are not one object"), err)
		}
	}
	return nil
}

func (leftover *linuxOutputProbeLeftover) remove() error {
	for _, name := range []string{"stage", "publication", "anchor", "record.tmp", "record"} {
		file := leftover.regular[name]
		if file == nil {
			continue
		}
		if err := leftover.directory.unlinkRegularFile(name, file); err != nil {
			return err
		}
	}
	for _, name := range []string{"candidate", "installed"} {
		directory := leftover.directories[name]
		if directory == nil {
			continue
		}
		if err := leftover.directory.unlinkDirectory(name, directory); err != nil {
			return err
		}
	}
	if err := leftover.closeChildren(); err != nil {
		return err
	}
	if err := leftover.root.unlinkDirectory(leftover.name, leftover.directory); err != nil {
		return err
	}
	return leftover.directory.close()
}

func (leftover *linuxOutputProbeLeftover) closeChildren() error {
	var result error
	for name, file := range leftover.regular {
		result = errors.Join(result, file.close())
		delete(leftover.regular, name)
	}
	for name, directory := range leftover.directories {
		result = errors.Join(result, directory.close())
		delete(leftover.directories, name)
	}
	return result
}

func (leftover *linuxOutputProbeLeftover) close() error {
	if leftover == nil {
		return nil
	}
	return errors.Join(leftover.closeChildren(), leftover.directory.close())
}
