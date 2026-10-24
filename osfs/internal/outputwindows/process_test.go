//go:build windows

package outputwindows

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	nativeOutputCrashRootEnvironment  = "WINDSHARE_NATIVE_OUTPUT_CRASH_ROOT"
	nativeOutputCrashReadyEnvironment = "WINDSHARE_NATIVE_OUTPUT_CRASH_READY"
	nativeOutputCrashReadyTimeout     = 20 * time.Second
	nativeOutputCrashPollInterval     = 10 * time.Millisecond
	nativeOutputCrashChildMaximumWait = 5 * time.Minute
)

func requireUnprivilegedWindowsNTFSCertification(t *testing.T) {
	t.Helper()
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("Windows/NTFS native certification is meaningful only as an ordinary receiver")
	}
}

func killNativeOutputChildAfterReady(t *testing.T, readyPath string, environment []string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	command.Stdin = bytes.NewReader(nil)
	command.Env = append(os.Environ(), environment...)
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	if err := command.Start(); err != nil {
		t.Fatalf("start native output crash child: %v", err)
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

	deadline := time.NewTimer(nativeOutputCrashReadyTimeout)
	ticker := time.NewTicker(nativeOutputCrashPollInterval)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case err := <-waited:
			childRunning = false
			t.Fatalf("native output crash child exited before its persisted cut: %v\n%s", err, childOutput.String())
		case <-deadline.C:
			t.Fatalf("native output crash child did not persist its cut within %s\n%s",
				nativeOutputCrashReadyTimeout, childOutput.String())
		case <-ticker.C:
			if _, err := os.Stat(readyPath); err == nil {
				if err := command.Process.Kill(); err != nil {
					t.Fatalf("kill native output crash child: %v", err)
				}
				if err := <-waited; err == nil {
					childRunning = false
					t.Fatal("native output crash child exited successfully instead of being terminated")
				}
				childRunning = false
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inspect native output crash-child marker: %v", err)
			}
		}
	}
}

func signalNativeOutputCrashCut(t *testing.T, readyPath string) {
	t.Helper()
	// The marker lives outside the output root, so it cannot make an
	// unpersisted output cut appear recoverable.
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("signal native output crash cut: %v", err)
	}
}
