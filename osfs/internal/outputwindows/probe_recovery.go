//go:build windows

package outputwindows

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/windows"
)

const (
	windowsOutputProbeLockName         = ".windshare-output.probe.lock"
	windowsOutputProbeReservedPrefix   = ".windshare-output.probe"
	windowsOutputProbeMaximumLeftovers = 64
	windowsOutputProbeMaximumEntries   = 7
	windowsOutputProbeMutexDomain      = "windshare/output-probe-mutex/v1"
	windowsOutputProbeMutexPrefix      = `Global\WindShare.OutputProbe.`
	windowsWaitObject                  = uint32(0)
	windowsWaitAbandoned               = uint32(0x80)
	windowsWaitTimeout                 = uint32(0x102)
)

var windowsOutputProbeRegularNames = map[string]struct{}{
	"stage": {}, "anchor": {}, "publication": {}, "record": {}, "record.tmp": {},
}

var windowsOutputProbeDirectoryNames = map[string]struct{}{
	"candidate": {}, "installed": {},
}

type windowsOutputProbeLock struct {
	handle       windows.Handle
	held         bool
	threadPinned bool
	threadID     uint32
}

func (directory *windowsDirectory) currentObjectClaim() ([]byte, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	facts, err := directory.inspector.Inspect(directory.handle())
	if err != nil {
		return nil, windowsFailure("claim current Windows output object", directory.path,
			errWindowsOutputUnsafe, err)
	}
	if err := windowsValidateOpenedObject(facts, directory.volume, true); err != nil {
		return nil, windowsFailure("claim current Windows output object", directory.path,
			errWindowsOutputUnsafe, err)
	}
	// This claim only keys an in-process probe mutex while the root is pinned.
	// It is deliberately not a restart identity and therefore must not enroll an
	// NTFS Object ID as a prerequisite for SafePublish or CrashCleanup.
	claim := make([]byte, len(facts.object.volume.guid)+8+len(facts.object.fileID))
	copy(claim, facts.object.volume.guid)
	offset := len(facts.object.volume.guid)
	binary.BigEndian.PutUint64(claim[offset:], facts.object.volume.serial)
	offset += 8
	copy(claim[offset:], facts.object.fileID[:])
	return claim, nil
}

func (root *windowsDirectory) acquireOutputProbeLock() (_ *windowsOutputProbeLock, resultErr error) {
	const operation = "lock Windows output feature probe"
	if err := root.verify(false); err != nil {
		return nil, err
	}
	rootClaim, err := root.currentObjectClaim()
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(windowsOutputProbeMutexDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(rootClaim)
	mutexName := windowsOutputProbeMutexPrefix + hex.EncodeToString(hash.Sum(nil))
	nativeName, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, windowsFailure(operation, mutexName, errWindowsOutputUnsafe, err)
	}
	descriptor, err := root.policy.descriptor(false)
	if err != nil {
		return nil, windowsFailure(operation, mutexName, errWindowsOutputUnsafe, err)
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
		return nil, windowsNativeOperationFailure(operation, mutexName, createErr)
	}
	if handle == windows.InvalidHandle || handle == 0 {
		return nil, windowsFailure(operation, mutexName, errWindowsOutputUnsafe,
			errors.New("kernel returned an invalid feature-probe mutex handle"))
	}
	if err := root.policy.verifyKernelMutex(handle); err != nil {
		return nil, errors.Join(
			windowsFailure(operation, mutexName, errWindowsOutputUnsafe,
				errors.New("root-bound feature-probe mutex has a non-canonical security envelope")),
			err,
			windows.CloseHandle(handle),
		)
	}
	wait, waitErr := windows.WaitForSingleObject(handle, 0)
	if waitErr != nil {
		return nil, errors.Join(
			windowsNativeOperationFailure(operation, mutexName, waitErr),
			windows.CloseHandle(handle),
		)
	}
	switch wait {
	case windowsWaitTimeout:
		return nil, errors.Join(
			windowsFailure(operation, mutexName, errWindowsOutputLockBusy,
				errors.New("root-bound feature-probe mutex is already held")),
			windows.CloseHandle(handle),
		)
	case windowsWaitObject, windowsWaitAbandoned:
	default:
		return nil, errors.Join(
			windowsFailure(operation, mutexName, errWindowsOutputUnsafe,
				fmt.Errorf("kernel returned unexpected mutex wait status %#x", wait)),
			windows.CloseHandle(handle),
		)
	}
	currentClaim, claimErr := root.currentObjectClaim()
	if claimErr != nil || !bytes.Equal(rootClaim, currentClaim) {
		return nil, errors.Join(
			windowsFailure(operation, mutexName, errWindowsOutputUnsafe,
				errors.New("output root changed while its feature-probe mutex was acquired")),
			claimErr,
			windows.ReleaseMutex(handle),
			windows.CloseHandle(handle),
		)
	}
	committed = true
	return &windowsOutputProbeLock{
		handle: handle, held: true, threadPinned: true, threadID: windows.GetCurrentThreadId(),
	}, nil
}

func (root *windowsDirectory) releaseOutputProbeLock(lock *windowsOutputProbeLock) error {
	const operation = "release Windows output feature probe"
	if lock == nil || !lock.held || lock.handle == windows.InvalidHandle || lock.handle == 0 {
		return windowsFailure(operation, windowsOutputProbeMutexPrefix, errWindowsOutputUnsafe,
			errors.New("feature-probe lock authority is absent"))
	}
	if !lock.threadPinned || lock.threadID == 0 || lock.threadID != windows.GetCurrentThreadId() {
		return windowsFailure(operation, windowsOutputProbeMutexPrefix, errWindowsOutputUnsafe,
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
		releaseErr = windowsNativeOperationFailure(operation, windowsOutputProbeMutexPrefix, releaseErr)
	}
	return errors.Join(releaseErr, windows.CloseHandle(handle))
}

func (root *windowsDirectory) recoverOutputProbeLeftovers() error {
	const operation = "recover Windows output feature probe"
	names, err := root.namesWithPrefix(
		windowsOutputProbeReservedPrefix,
		windowsOutputProbeMaximumLeftovers+1,
	)
	if err != nil {
		return err
	}
	leftovers := make([]*windowsOutputProbeLeftover, 0, len(names))
	closeAll := func() error {
		var closeErr error
		for _, leftover := range leftovers {
			closeErr = errors.Join(closeErr, leftover.close())
		}
		return closeErr
	}
	for _, name := range names {
		if !windowsCanonicalProbeName(name) {
			return errors.Join(windowsFailure(operation, name, errWindowsOutputUnsafe,
				errors.New("malformed reserved probe name blocks the output root")), closeAll())
		}
		if len(leftovers) == windowsOutputProbeMaximumLeftovers {
			return errors.Join(windowsFailure(operation, name, errWindowsOutputUnsafe,
				errors.New("probe leftover count exceeds its safety bound")), closeAll())
		}
		leftover, inspectErr := root.inspectOutputProbeLeftover(name)
		if inspectErr != nil {
			return errors.Join(windowsFailure(operation, name, errWindowsOutputUnsafe,
				errors.New("probe leftover does not match the strict temporary schema")), inspectErr, closeAll())
		}
		leftovers = append(leftovers, leftover)
	}
	for _, leftover := range leftovers {
		if err := leftover.remove(); err != nil {
			return errors.Join(windowsFailure(operation, leftover.name, errWindowsOutputUnsafe,
				errors.New("fixed probe leftover could not be reduced safely")), err, closeAll())
		}
	}
	return closeAll()
}

func windowsCanonicalProbeName(name string) bool {
	if !strings.HasPrefix(name, windowsOutputProbePrefix) ||
		len(name) != len(windowsOutputProbePrefix)+windowsOutputProbeRandomBytes*2 {
		return false
	}
	for _, character := range name[len(windowsOutputProbePrefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

type windowsOutputProbeLeftover struct {
	root        *windowsDirectory
	name        string
	directory   *windowsDirectory
	regular     map[string]*windowsFile
	directories map[string]*windowsDirectory
}

func (root *windowsDirectory) inspectOutputProbeLeftover(name string) (*windowsOutputProbeLeftover, error) {
	directory, err := root.OpenPrivateDirectory(name)
	if err != nil {
		return nil, err
	}
	leftover := &windowsOutputProbeLeftover{
		root: root, name: name, directory: directory,
		regular: make(map[string]*windowsFile), directories: make(map[string]*windowsDirectory),
	}
	var observation outputprobe.Observation
	fail := func(cause error) (*windowsOutputProbeLeftover, error) {
		return nil, errors.Join(cause, leftover.close())
	}
	names, err := directory.names(windowsOutputProbeMaximumEntries)
	if err != nil {
		return fail(err)
	}
	for _, entry := range names {
		if _, ok := windowsOutputProbeRegularNames[entry]; ok {
			file, openErr := directory.OpenPrivateFile(entry)
			if openErr != nil {
				return fail(openErr)
			}
			size, sizeErr := file.Size()
			if sizeErr != nil {
				return fail(errors.Join(fmt.Errorf("inspect probe file %q", entry), sizeErr, file.Close()))
			}
			if observeErr := observation.ObserveFile(entry, size); observeErr != nil {
				return fail(errors.Join(observeErr, file.Close()))
			}
			leftover.regular[entry] = file
			continue
		}
		if _, ok := windowsOutputProbeDirectoryNames[entry]; ok {
			child, openErr := directory.OpenPrivateDirectory(entry)
			if openErr != nil {
				return fail(openErr)
			}
			childNames, enumerateErr := child.names(0)
			if enumerateErr != nil || len(childNames) != 0 {
				return fail(errors.Join(fmt.Errorf("probe directory %q is not empty", entry), enumerateErr, child.Close()))
			}
			if observeErr := observation.ObserveDirectory(entry); observeErr != nil {
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
	if err := outputprobe.Validate(outputprobe.WindowsNTFS, observation); err != nil {
		return fail(err)
	}
	return leftover, nil
}

func (leftover *windowsOutputProbeLeftover) validateDataLinks() error {
	var witness *windowsFile
	for _, name := range []string{"stage", "anchor", "publication"} {
		file := leftover.regular[name]
		if file == nil {
			continue
		}
		if witness == nil {
			witness = file
			continue
		}
		same, err := sameWindowsOpenedObject(witness, file)
		if err != nil || !same {
			return errors.Join(errors.New("probe stage, anchor, and publication are not one object"), err)
		}
	}
	return nil
}

func (leftover *windowsOutputProbeLeftover) remove() error {
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

func (leftover *windowsOutputProbeLeftover) closeChildren() error {
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

func (leftover *windowsOutputProbeLeftover) close() error {
	if leftover == nil {
		return nil
	}
	return errors.Join(leftover.closeChildren(), leftover.directory.Close())
}
