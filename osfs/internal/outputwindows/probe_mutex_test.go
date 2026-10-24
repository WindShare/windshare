//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const windowsNativeProbeMutexChildEnvironment = "WINDSHARE_WINDOWS_PROBE_MUTEX_CHILD"

func TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment(t *testing.T) {
	if os.Getenv(windowsNativeProbeMutexChildEnvironment) == "1" {
		runWindowsNativeProbeMutexChild(t)
		return
	}
	requireUnprivilegedWindowsNTFSCertification(t)

	root := t.TempDir()
	platform, err := openWindowsV3OutputPlatform(root)
	if errors.Is(err, errWindowsV3OutputUnsupported) {
		t.Skipf("test volume is outside the local NTFS matrix: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	// The feature probe mutates a resumable output root, so it must run through
	// the same scoped ancestry guard used by production operations. Calling the
	// bare root would exercise an authority object that is intentionally not
	// certified for public output.
	probeGuard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := probeGuard.Root().probeRecoverableFeatures(); err != nil {
		t.Fatal(errors.Join(err, probeGuard.Close()))
	}
	if err := probeGuard.Close(); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(t.TempDir(), "mutex-child.ready")
	command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	command.Env = append(os.Environ(),
		windowsNativeProbeMutexChildEnvironment+"=1",
		nativeOutputCrashRootEnvironment+"="+root,
		nativeOutputCrashReadyEnvironment+"="+readyPath,
	)
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatalf("start feature-probe mutex child: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	childRunning := true
	defer func() {
		if childRunning {
			_ = command.Process.Kill()
			<-waited
		}
	}()
	waitForWindowsNativeProbeMutexChild(t, readyPath, waited, &childRunning, &childOutput)

	busyGuard := prepareWindowsNativeProbeGuard(t, platform)
	busyRoot := busyGuard.Root()
	lock, lockErr := busyRoot.acquireOutputProbeLock()
	if lock != nil {
		lockErr = errors.Join(lockErr, busyRoot.releaseOutputProbeLock(lock))
	}
	busyGuardCloseErr := busyGuard.Close()
	if !errors.Is(lockErr, errWindowsV3OutputLockBusy) || busyGuardCloseErr != nil {
		t.Fatalf("second process acquired held feature-probe mutex: %v", errors.Join(lockErr, busyGuardCloseErr))
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("terminate feature-probe mutex owner: %v", err)
	}
	if err := <-waited; err == nil {
		childRunning = false
		t.Fatal("feature-probe mutex child exited successfully instead of being terminated")
	}
	childRunning = false

	recoveredGuard := prepareWindowsNativeProbeGuard(t, platform)
	recoveredRoot := recoveredGuard.Root()
	recoveredLock, recoveredErr := recoveredRoot.acquireOutputProbeLock()
	if recoveredErr != nil {
		t.Fatalf("acquire abandoned feature-probe mutex: %v", errors.Join(recoveredErr, recoveredGuard.Close()))
	}
	if err := errors.Join(recoveredRoot.releaseOutputProbeLock(recoveredLock), recoveredGuard.Close()); err != nil {
		t.Fatalf("release recovered feature-probe mutex: %v", err)
	}
}

func prepareWindowsNativeProbeGuard(
	t *testing.T,
	platform *windowsV3OutputPlatform,
) *windowsV3PublicOperationGuard {
	t.Helper()
	if platform == nil {
		t.Fatal("Windows feature-probe platform is absent")
	}
	guard, err := platform.acquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	root := guard.Root()
	if root == nil {
		t.Fatal(errors.Join(
			errors.New("windows feature-probe guard has no root authority"),
			guard.Close(),
		))
	}
	if _, err := root.prepareIdentityClaim(); err != nil {
		t.Fatal(errors.Join(err, guard.Close()))
	}
	return guard
}

func runWindowsNativeProbeMutexChild(t *testing.T) {
	root := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	if root == "" || readyPath == "" {
		t.Fatal("feature-probe mutex child parameters are absent")
	}
	platform, err := openWindowsV3OutputPlatform(root)
	if err != nil {
		t.Fatal(err)
	}
	guard := prepareWindowsNativeProbeGuard(t, platform)
	lock, err := guard.Root().acquireOutputProbeLock()
	if err != nil {
		t.Fatal(errors.Join(err, guard.Close()))
	}
	signalNativeOutputCrashCut(t, readyPath)
	time.Sleep(nativeOutputCrashChildMaximumWait)
	runtime.KeepAlive(lock)
	runtime.KeepAlive(guard)
	t.Fatal("feature-probe mutex child was not terminated by its parent")
}

func waitForWindowsNativeProbeMutexChild(
	t *testing.T,
	readyPath string,
	waited <-chan error,
	childRunning *bool,
	childOutput *bytes.Buffer,
) {
	t.Helper()
	deadline := time.NewTimer(nativeOutputCrashReadyTimeout)
	ticker := time.NewTicker(nativeOutputCrashPollInterval)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-waited:
			*childRunning = false
			t.Fatalf("feature-probe mutex child exited before acquiring the mutex: %v\n%s", err, childOutput.String())
		case <-deadline.C:
			t.Fatalf("feature-probe mutex child did not acquire the mutex within %s\n%s",
				nativeOutputCrashReadyTimeout, childOutput.String())
		case <-ticker.C:
			if _, err := os.Stat(readyPath); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inspect feature-probe mutex child marker: %v", err)
			}
		}
	}
}
