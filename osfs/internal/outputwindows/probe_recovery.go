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
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/outputprobe"
	"golang.org/x/sys/windows"
)

const (
	windowsOutputProbeLockName    = ".windshare-output.probe.lock"
	windowsOutputProbeMutexDomain = "windshare/output-probe-mutex/v1"
	windowsOutputProbeMutexPrefix = `Global\WindShare.OutputProbe.`
	windowsWaitObject             = uint32(0)
	windowsWaitAbandoned          = uint32(0x80)
	windowsWaitTimeout            = uint32(0x102)
)

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
	names, err := root.namesWithPrefix(outputprobe.ReservedPrefix, outputprobe.MaximumLeftovers+1)
	if err != nil {
		return err
	}
	return windowsProbeError(outputprobe.Recover(&windowsOutputDirectory{native: root}, names, outputprobe.SizedDataLinks))
}
