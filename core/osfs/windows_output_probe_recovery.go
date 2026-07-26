//go:build windows

package osfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsV3OutputProbeLockName         = ".windshare-output.probe.lock"
	windowsV3OutputProbeReservedPrefix   = ".windshare-output.probe"
	windowsV3OutputProbeMaximumLeftovers = 64
	windowsV3OutputProbeMaximumEntries   = 7
	windowsV3OutputProbeMutexDomain      = "windshare/output-probe-mutex/v1"
	windowsV3OutputProbeMutexPrefix      = `Global\WindShare.OutputProbe.`
	windowsV3WaitObject                  = uint32(0)
	windowsV3WaitAbandoned               = uint32(0x80)
	windowsV3WaitTimeout                 = uint32(0x102)
)

var windowsV3OutputProbeRegularNames = map[string]struct{}{
	"stage": {}, "anchor": {}, "publication": {}, "record": {}, "record.tmp": {},
}

var windowsV3OutputProbeDirectoryNames = map[string]struct{}{
	"candidate": {}, "installed": {},
}

type windowsV3OutputProbeLock struct {
	handle       windows.Handle
	held         bool
	threadPinned bool
	threadID     uint32
}

func (root *windowsV3Directory) acquireOutputProbeLock() (_ *windowsV3OutputProbeLock, resultErr error) {
	const operation = "lock Windows output feature probe"
	if err := root.verify(false); err != nil {
		return nil, err
	}
	rootClaim, err := root.identityClaim()
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(windowsV3OutputProbeMutexDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(rootClaim)
	mutexName := windowsV3OutputProbeMutexPrefix + hex.EncodeToString(hash.Sum(nil))
	nativeName, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe, err)
	}
	descriptor, err := root.policy.descriptor(false)
	if err != nil {
		return nil, windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe, err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	// Windows mutex ownership belongs to an OS thread, not a goroutine. Pinning
	// across the complete probe guarantees the deferred release executes as the
	// kernel owner even when the Go scheduler would otherwise migrate it.
	runtime.LockOSThread()
	committed := false
	defer func() {
		if !committed {
			runtime.UnlockOSThread()
		}
	}()
	handle, createErr := windows.CreateMutex(&attributes, false, nativeName)
	runtime.KeepAlive(descriptor)
	if createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		return nil, windowsV3NativeOperationFailure(operation, mutexName, createErr)
	}
	if handle == windows.InvalidHandle || handle == 0 {
		return nil, windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe,
			errors.New("kernel returned an invalid feature-probe mutex handle"))
	}
	if err := root.policy.verifyKernelMutex(handle); err != nil {
		return nil, errors.Join(
			windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe,
				errors.New("root-bound feature-probe mutex has a non-canonical security envelope")),
			err,
			windows.CloseHandle(handle),
		)
	}
	wait, waitErr := windows.WaitForSingleObject(handle, 0)
	if waitErr != nil {
		return nil, errors.Join(
			windowsV3NativeOperationFailure(operation, mutexName, waitErr),
			windows.CloseHandle(handle),
		)
	}
	switch wait {
	case windowsV3WaitTimeout:
		return nil, errors.Join(
			windowsV3Failure(operation, mutexName, errWindowsV3OutputLockBusy,
				errors.New("root-bound feature-probe mutex is already held")),
			windows.CloseHandle(handle),
		)
	case windowsV3WaitObject, windowsV3WaitAbandoned:
	default:
		return nil, errors.Join(
			windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe,
				fmt.Errorf("kernel returned unexpected mutex wait status %#x", wait)),
			windows.CloseHandle(handle),
		)
	}
	currentClaim, claimErr := root.identityClaim()
	if claimErr != nil || !bytes.Equal(rootClaim, currentClaim) {
		return nil, errors.Join(
			windowsV3Failure(operation, mutexName, errWindowsV3OutputUnsafe,
				errors.New("output root changed while its feature-probe mutex was acquired")),
			claimErr,
			windows.ReleaseMutex(handle),
			windows.CloseHandle(handle),
		)
	}
	committed = true
	return &windowsV3OutputProbeLock{
		handle: handle, held: true, threadPinned: true, threadID: windows.GetCurrentThreadId(),
	}, nil
}

func (root *windowsV3Directory) releaseOutputProbeLock(lock *windowsV3OutputProbeLock) error {
	const operation = "release Windows output feature probe"
	if lock == nil || !lock.held || lock.handle == windows.InvalidHandle || lock.handle == 0 {
		return windowsV3Failure(operation, windowsV3OutputProbeMutexPrefix, errWindowsV3OutputUnsafe,
			errors.New("feature-probe lock authority is absent"))
	}
	if !lock.threadPinned || lock.threadID == 0 || lock.threadID != windows.GetCurrentThreadId() {
		return windowsV3Failure(operation, windowsV3OutputProbeMutexPrefix, errWindowsV3OutputUnsafe,
			errors.New("feature-probe mutex release moved away from its owning OS thread"))
	}
	defer runtime.UnlockOSThread()
	handle := lock.handle
	lock.held = false
	lock.threadPinned = false
	lock.threadID = 0
	lock.handle = windows.InvalidHandle
	releaseErr := windows.ReleaseMutex(handle)
	if releaseErr != nil {
		releaseErr = windowsV3NativeOperationFailure(operation, windowsV3OutputProbeMutexPrefix, releaseErr)
	}
	return errors.Join(releaseErr, windows.CloseHandle(handle))
}

func (root *windowsV3Directory) recoverOutputProbeLeftovers() error {
	const operation = "recover Windows output feature probe"
	names, err := root.namesWithPrefix(
		windowsV3OutputProbeReservedPrefix,
		windowsV3OutputProbeMaximumLeftovers+1,
	)
	if err != nil {
		return err
	}
	leftovers := make([]*windowsV3OutputProbeLeftover, 0, len(names))
	closeAll := func() error {
		var closeErr error
		for _, leftover := range leftovers {
			closeErr = errors.Join(closeErr, leftover.close())
		}
		return closeErr
	}
	for _, name := range names {
		if !windowsV3CanonicalProbeName(name) {
			return errors.Join(windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
				errors.New("malformed reserved probe name blocks the output root")), closeAll())
		}
		if len(leftovers) == windowsV3OutputProbeMaximumLeftovers {
			return errors.Join(windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
				errors.New("probe leftover count exceeds its safety bound")), closeAll())
		}
		leftover, inspectErr := root.inspectOutputProbeLeftover(name)
		if inspectErr != nil {
			return errors.Join(windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
				errors.New("probe leftover does not match the strict temporary schema")), inspectErr, closeAll())
		}
		leftovers = append(leftovers, leftover)
	}
	for _, leftover := range leftovers {
		if err := leftover.remove(); err != nil {
			return errors.Join(windowsV3Failure(operation, leftover.name, errWindowsV3OutputUnsafe,
				errors.New("fixed probe leftover could not be reduced safely")), err, closeAll())
		}
	}
	return closeAll()
}

func windowsV3CanonicalProbeName(name string) bool {
	if !strings.HasPrefix(name, windowsV3OutputProbePrefix) ||
		len(name) != len(windowsV3OutputProbePrefix)+windowsV3OutputProbeRandomBytes*2 {
		return false
	}
	for _, character := range name[len(windowsV3OutputProbePrefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type windowsV3OutputProbeLeftover struct {
	root        *windowsV3Directory
	name        string
	directory   *windowsV3Directory
	regular     map[string]*windowsV3File
	directories map[string]*windowsV3Directory
}

func (root *windowsV3Directory) inspectOutputProbeLeftover(name string) (*windowsV3OutputProbeLeftover, error) {
	directory, err := root.OpenPrivateDirectory(name)
	if err != nil {
		return nil, err
	}
	leftover := &windowsV3OutputProbeLeftover{
		root: root, name: name, directory: directory,
		regular: make(map[string]*windowsV3File), directories: make(map[string]*windowsV3Directory),
	}
	var observation outputV3ProbeCutObservation
	fail := func(cause error) (*windowsV3OutputProbeLeftover, error) {
		return nil, errors.Join(cause, leftover.close())
	}
	names, err := directory.names(windowsV3OutputProbeMaximumEntries)
	if err != nil {
		return fail(err)
	}
	for _, entry := range names {
		if _, ok := windowsV3OutputProbeRegularNames[entry]; ok {
			file, openErr := directory.OpenPrivateFile(entry)
			if openErr != nil {
				return fail(openErr)
			}
			size, sizeErr := file.Size()
			if sizeErr != nil {
				return fail(errors.Join(fmt.Errorf("inspect probe file %q", entry), sizeErr, file.Close()))
			}
			if observeErr := observation.observeFile(entry, size); observeErr != nil {
				return fail(errors.Join(observeErr, file.Close()))
			}
			leftover.regular[entry] = file
			continue
		}
		if _, ok := windowsV3OutputProbeDirectoryNames[entry]; ok {
			child, openErr := directory.OpenPrivateDirectory(entry)
			if openErr != nil {
				return fail(openErr)
			}
			childNames, enumerateErr := child.names(0)
			if enumerateErr != nil || len(childNames) != 0 {
				return fail(errors.Join(fmt.Errorf("probe directory %q is not empty", entry), enumerateErr, child.Close()))
			}
			if observeErr := observation.observeDirectory(entry); observeErr != nil {
				return fail(errors.Join(observeErr, child.Close()))
			}
			leftover.directories[entry] = child
			continue
		}
		return fail(fmt.Errorf("unexpected probe entry %q", entry))
	}
	if err := leftover.validateDataLinks(); err != nil {
		return fail(err)
	}
	if err := validateOutputV3ProbeCut(outputV3ProbeDataWindowsNTFS, observation); err != nil {
		return fail(err)
	}
	return leftover, nil
}

func (leftover *windowsV3OutputProbeLeftover) validateDataLinks() error {
	var witness *windowsV3File
	for _, name := range []string{"stage", "anchor", "publication"} {
		file := leftover.regular[name]
		if file == nil {
			continue
		}
		if witness == nil {
			witness = file
			continue
		}
		same, err := sameWindowsV3OpenedObject(witness, file)
		if err != nil || !same {
			return errors.Join(errors.New("probe stage, anchor, and publication are not one object"), err)
		}
	}
	return nil
}

func (leftover *windowsV3OutputProbeLeftover) remove() error {
	for _, name := range []string{"stage", "publication", "anchor", "record.tmp", "record"} {
		file := leftover.regular[name]
		if file == nil {
			continue
		}
		if err := leftover.directory.RemoveRegularLink(name, file); err != nil {
			return err
		}
		if err := leftover.directory.Sync(); err != nil {
			return err
		}
	}
	for _, name := range []string{"candidate", "installed"} {
		directory := leftover.directories[name]
		if directory == nil {
			continue
		}
		if err := leftover.directory.RemoveDirectory(name, directory); err != nil {
			return err
		}
		if err := leftover.directory.Sync(); err != nil {
			return err
		}
	}
	if err := leftover.closeChildren(); err != nil {
		return err
	}
	if err := leftover.root.RemoveDirectory(leftover.name, leftover.directory); err != nil {
		return err
	}
	if err := leftover.root.Sync(); err != nil {
		return err
	}
	return leftover.directory.Close()
}

func (leftover *windowsV3OutputProbeLeftover) closeChildren() error {
	var result error
	for name, file := range leftover.regular {
		result = errors.Join(result, file.Close())
		delete(leftover.regular, name)
	}
	for name, directory := range leftover.directories {
		result = errors.Join(result, directory.Close())
		delete(leftover.directories, name)
	}
	return result
}

func (leftover *windowsV3OutputProbeLeftover) close() error {
	if leftover == nil {
		return nil
	}
	return errors.Join(leftover.closeChildren(), leftover.directory.Close())
}
