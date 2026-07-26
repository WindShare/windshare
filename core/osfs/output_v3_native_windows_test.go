//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

const (
	windowsNTFSNativeCertificationProfile   = "windows-ntfs"
	windowsNativeProbeMutexChildEnvironment = "WINDSHARE_WINDOWS_PROBE_MUTEX_CHILD"
)

func TestWindowsNTFSNativeCertification(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	platform := openCertifiedNativeOutputForTest(
		t,
		t.TempDir(),
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
	)
	if err := platform.Close(); err != nil {
		t.Fatalf("close Windows/NTFS certification authority: %v", err)
	}
}

func TestWindowsNTFSProcessRestartRecovery(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	runNativeOutputProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
		windowsV3OutputProbePrefix+"27182818284590452353602874713526",
		1,
	)
	// CreatePrivateFile is durable before Truncate. Killing at size zero proves
	// recovery accepts that exact pre-truncate cut without broadening later
	// hard-link states to zero-length data objects.
	runNativeOutputProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
		windowsV3OutputProbePrefix+"27182818284590452353602874713526",
		0,
	)
	runNativeOutputSessionProcessRestartRecoveryTest(
		t,
		windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
	)
}

func requireUnprivilegedWindowsNTFSCertification(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		return
	}
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == windowsNTFSNativeCertificationProfile {
		t.Fatal("required Windows/NTFS certification must run as an ordinary non-elevated receiver")
	}
	if os.Getenv(windowsNativeProbeMutexChildEnvironment) == "1" {
		t.Fatal("Windows feature-probe mutex child unexpectedly runs elevated")
	}
	t.Skip("Windows/NTFS native certification is meaningful only as an ordinary receiver")
}

func TestWindowsNTFSResumeListingIsReadOnlyBeforeRootIdentityBootstrap(t *testing.T) {
	t.Run("empty-root", func(t *testing.T) {
		root := t.TempDir()
		authority := newNativeOutputSessionAuthority(t, root)
		trap := trapWindowsV3ObjectIDMutation(authority)
		inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		)
		if err != nil {
			t.Fatalf("list unused NTFS root: %v", err)
		}
		if summaries := inventory.Summaries(); len(summaries) != 0 {
			t.Errorf("unused NTFS root summaries = %+v", summaries)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
		if calls := trap.calls.Load(); calls != 0 {
			t.Fatalf("empty-root listing invoked CreateOrGet %d times", calls)
		}
	})

	t.Run("legacy-only-root", func(t *testing.T) {
		root := t.TempDir()
		journalName := legacyOutputStatePrefix +
			strings.Repeat("11", transfer.OutputSessionIdentityBytes) + legacyOutputJournalSuffix
		if err := os.WriteFile(
			filepath.Join(root, journalName), v3RecoveryLegacyV2JournalBytes(t), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		authority := newNativeOutputSessionAuthority(t, root)
		trap := trapWindowsV3ObjectIDMutation(authority)
		inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		)
		if err != nil {
			t.Fatalf("list legacy-only NTFS root: %v", err)
		}
		defer inventory.Close()
		summaries := inventory.Summaries()
		if len(summaries) != 1 || summaries[0].Reference.Kind() != ResumeStateLegacyUntrusted ||
			v3RecoveryHasAttention(summaries[0], "legacy-v2-root-pin-unavailable") {
			t.Fatalf("legacy-only NTFS summaries = %+v", summaries)
		}
		item, found := inventory.items[summaries[0].Reference.itemID]
		if !found || !item.authority.legacyRemovable || item.authority.legacyPin == nil ||
			item.authority.legacyRoot == nil {
			t.Fatalf("legacy-only NTFS inventory did not retain native discard authority: %+v", item.authority)
		}
		if calls := trap.calls.Load(); calls != 0 {
			t.Fatalf("legacy-only listing invoked CreateOrGet %d times", calls)
		}
	})
}

func TestWindowsNTFSProbeMutexIsProcessExclusiveAndRecoversAbandonment(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	if os.Getenv(windowsNativeProbeMutexChildEnvironment) == "1" {
		runWindowsNativeProbeMutexChild(t)
		return
	}

	root := t.TempDir()
	platform := openCertifiedNativeOutputForTest(
		t, root, windowsNTFSNativeCertificationProfile,
		resumestate.CertificationWindowsNTFSProcessRestart,
	)
	defer platform.Close()
	windowsPlatform, ok := platform.(*windowsOutputV3Platform)
	if !ok || windowsPlatform.native == nil || windowsPlatform.native.root == nil {
		t.Fatal("certified Windows platform has no native root authority")
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

	busyGuard := prepareWindowsNativeProbeGuard(t, windowsPlatform.native)
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

	recoveredGuard := prepareWindowsNativeProbeGuard(t, windowsPlatform.native)
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
			errors.New("Windows feature-probe guard has no root authority"),
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
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	windowsPlatform, ok := platform.(*windowsOutputV3Platform)
	if !ok || windowsPlatform.native == nil || windowsPlatform.native.root == nil {
		t.Fatal("feature-probe mutex child has no native root authority")
	}
	guard := prepareWindowsNativeProbeGuard(t, windowsPlatform.native)
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

func TestWindowsNTFSCreatesMissingRootThroughCertifiedHandles(t *testing.T) {
	target := filepath.Join(t.TempDir(), "first", "second")
	platform, err := openOutputV3Platform(target, true)
	if err != nil {
		nativeOutputCertificationFailure(t, windowsNTFSNativeCertificationProfile,
			"create missing Windows/NTFS root through certified handles", err)
		return
	}
	defer func() {
		if err := platform.Close(); err != nil {
			t.Errorf("close handle-created Windows/NTFS root: %v", err)
		}
	}()
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		t.Fatalf("prepare persistent Windows/NTFS root identity: %v", err)
	}
	if binding, err := platform.RootBinding(); err != nil || binding.IsZero() {
		t.Fatalf("bind handle-created Windows/NTFS root: zero=%t err=%v", binding.IsZero(), err)
	}
}

func TestWindowsRootCreationDoesNotTraverseReparseAncestor(t *testing.T) {
	carrier := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(carrier, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skipf("create Windows reparse adversary: %v", err)
	}
	unexpected := filepath.Join(outside, "must-not-exist")
	platform, err := openOutputV3Platform(filepath.Join(alias, "must-not-exist"), true)
	if platform != nil {
		_ = platform.Close()
	}
	if err == nil {
		t.Fatal("root creation traversed a reparse ancestor")
	}
	if _, statErr := os.Lstat(unexpected); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reparse target was mutated before certification: %v", statErr)
	}
}
