package checkpointcleaner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	cleanerCrashHelperEnv = "WINDSHARE_TEST_CLEANER_CRASH_HELPER"
	cleanerCrashRootEnv   = "WINDSHARE_TEST_CLEANER_CRASH_ROOT"
)

func TestCleanerRejectsUnownedRootPrefixWithoutManufacturingOwnership(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	foreign := filepath.Join(rootPath, legacyRootPrefix+"foreign.journal")
	if err := os.WriteFile(foreign, []byte("not WindShare-owned"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil {
		t.Fatal(err)
	}
	if !report.NeedsAttention() || report.Status != CheckpointCleanupStatusNeedsAttention {
		t.Fatalf("foreign root entry report = %+v", report)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign root entry was mutated: %v", err)
	}
	status, err := inspectCleanerOwnership(platform)
	if err != nil || status != checkpointstore.OwnershipAbsent {
		t.Fatalf("cleanup manufactured ownership: status=%d err=%v", status, err)
	}
}

func TestCleanerBindsOwnershipToCertifiedRootAndRescansCompletedState(t *testing.T) {
	t.Run("mismatched marker", func(t *testing.T) {
		platform, _ := newCleanerPlatform(t)
		if err := checkpointstore.BootstrapOwnership(checkpointstore.NamespaceConfig{
			Root: platform.Root(), BackendID: transfer.NativeFilesystemOutputBackendID,
			RootIdentity: bytes.Repeat([]byte{0x7a}, resumestate.OutputRootBindingBytes),
		}); err != nil {
			t.Fatal(err)
		}
		report, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
		if err != nil {
			t.Fatal(err)
		}
		if !report.NeedsAttention() {
			t.Fatalf("mismatched certified-root marker report = %+v", report)
		}
	})

	t.Run("completed state is not a scan cache", func(t *testing.T) {
		platform, rootPath := newCleanerPlatform(t)
		first, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
		if err != nil || !first.Complete {
			t.Fatalf("initial cleanup = %+v, %v", first, err)
		}
		foreign := filepath.Join(rootPath, legacyRootPrefix+"arrived-later")
		if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
		second, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
		if err != nil {
			t.Fatal(err)
		}
		if !second.NeedsAttention() || second.Complete {
			t.Fatalf("completed cleanup skipped rescan: %+v", second)
		}
	})
}

func TestCleanerResumesExactOwnershipCandidate(t *testing.T) {
	platform, _ := newCleanerPlatform(t)
	binding, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := resumestate.NewFileCheckpointOwnership(
		string(transfer.NativeFilesystemOutputBackendID), binding.Bytes(),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	control, err := platform.Root().CreateDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	checkpointRoot, err := control.CreateDirectory(resumestate.CheckpointsDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpointRoot.Close()
	candidate := checkpointstore.TemporaryName(checkpointstore.OwnershipFile, encoded, 0)
	writeCapabilityFile(t, checkpointRoot, candidate, encoded)
	if err := errors.Join(checkpointRoot.Sync(), control.Sync(), platform.Root().Sync()); err != nil {
		t.Fatal(err)
	}

	status, err := inspectCleanerOwnership(platform)
	if err != nil || status != checkpointstore.OwnershipRecoverable {
		t.Fatalf("ownership candidate status = %d, %v", status, err)
	}
	report, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !report.Complete || report.NeedsAttention() {
		t.Fatalf("ownership candidate cleanup = %+v, %v", report, err)
	}
	status, err = inspectCleanerOwnership(platform)
	if err != nil || status != checkpointstore.OwnershipMatched {
		t.Fatalf("recovered ownership status = %d, %v", status, err)
	}
}

func TestCleanerCrashCutResumesUnderCoordinatorAndSessionExclusion(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	bootstrapCleaner(t, platform)
	legacy := installLegacyNamespace(t, platform)
	injected := errors.New("injected cleanup crash cut")
	faulted := cleanerConfig(platform)
	faulted.Fault = func(step CheckpointCleanupStep) error {
		if step.Index == 0 {
			return injected
		}
		return nil
	}
	if _, err := CleanOwnedNamespace(context.Background(), faulted); !errors.Is(err, injected) {
		t.Fatalf("faulted cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
		t.Fatalf("fault cut mutated payload: %v", err)
	}

	resumed, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !resumed.Complete || !resumed.Resumed || resumed.Removed == 0 {
		t.Fatalf("resumed cleanup = %+v, %v", resumed, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.session)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired session remains after resume: %v", err)
	}
}

func TestCleanerRefusesActiveLegacyWriters(t *testing.T) {
	for _, test := range []struct {
		name string
		lock func(*testing.T, outputcap.Platform, legacyFixture) outputcap.Lock
	}{
		{name: "coordinator", lock: lockLegacyCoordinator},
		{name: "session", lock: lockLegacySession},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, rootPath := newCleanerPlatform(t)
			bootstrapCleaner(t, platform)
			legacy := installLegacyNamespace(t, platform)
			lock := test.lock(t, platform, legacy)
			defer lock.Close()
			if _, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform)); !errors.Is(err, ErrCheckpointCleanerBusy) {
				t.Fatalf("cleanup with active %s = %v", test.name, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
				t.Fatalf("cleanup crossed active %s: %v", test.name, err)
			}
		})
	}
}

func TestCleanerRevalidatesCertifiedRootBeforeEveryRemoval(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	bootstrapCleaner(t, platform)
	legacy := installLegacyNamespace(t, platform)
	replaced := errors.New("certified root changed")
	changing := &changingBindingPlatform{Platform: platform, failAt: 4, failure: replaced}
	if _, err := CleanOwnedNamespace(context.Background(), cleanerConfig(changing)); !errors.Is(err, ErrCheckpointCleanerOwnership) || !errors.Is(err, replaced) {
		t.Fatalf("root replacement cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
		t.Fatalf("mutation occurred after root replacement: %v", err)
	}
}

func TestCleanupLockReleasedAfterProcessCrash(t *testing.T) {
	if os.Getenv(cleanerCrashHelperEnv) == "1" {
		runCleanerLockCrashHelper(t, os.Getenv(cleanerCrashRootEnv))
		return
	}
	platform, rootPath := newCleanerPlatform(t)
	bootstrapCleaner(t, platform)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCleanupLockReleasedAfterProcessCrash$")
	command.Env = append(os.Environ(), cleanerCrashHelperEnv+"=1", cleanerCrashRootEnv+"="+rootPath)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "cleaner-lock-acquired" {
				close(locked)
				return
			}
		}
	}()
	select {
	case <-locked:
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("crash helper did not acquire cleanup lock")
	}
	if _, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform)); !errors.Is(err, ErrCheckpointCleanerBusy) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("live helper cleanup = %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	recovered, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !recovered.Complete {
		t.Fatalf("cleanup after process crash = %+v, %v", recovered, err)
	}
}

type legacyFixture struct {
	session string
	payload string
}

func newCleanerPlatform(t *testing.T) (outputcap.Platform, string) {
	t.Helper()
	fixture := testoutputroot.New(t)
	platform, err := openCleanerTestPlatform(fixture.RootPath, fixture.CreateRoot)
	if err != nil {
		t.Skipf("native output platform unavailable: %v", err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	return platform, fixture.RootPath
}

func cleanerConfig(platform outputcap.Platform) OneShotCheckpointCleanerConfig {
	return OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: transfer.NativeFilesystemOutputBackendID,
	}
}

func inspectCleanerOwnership(platform outputcap.Platform) (checkpointstore.OwnershipStatus, error) {
	binding, err := platform.RootBinding()
	if err != nil {
		return 0, err
	}
	return checkpointstore.InspectOwnership(checkpointstore.NamespaceConfig{
		Root: platform.Root(), BackendID: transfer.NativeFilesystemOutputBackendID,
		RootIdentity: binding.Bytes(),
	})
}

func bootstrapCleaner(t *testing.T, platform outputcap.Platform) {
	t.Helper()
	report, err := CleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !report.Complete {
		t.Fatalf("bootstrap cleaner = %+v, %v", report, err)
	}
}

func installLegacyNamespace(t *testing.T, platform outputcap.Platform) legacyFixture {
	t.Helper()
	control, err := platform.Root().OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	installUnlockedLock(t, control, resumestate.CoordinatorLockName)
	sessions, err := control.CreateDirectory(resumestate.SessionsDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	sessionName := "retired-session"
	session, err := sessions.CreateDirectory(sessionName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	installUnlockedLock(t, session, resumestate.SessionLockName)
	writeCapabilityFile(t, session, "payload.bin", []byte("retired"))
	if err := errors.Join(session.Sync(), sessions.Sync(), control.Sync()); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName, sessionName)
	return legacyFixture{session: base, payload: filepath.Join(base, "payload.bin")}
}

func installUnlockedLock(t *testing.T, directory outputcap.Directory, name string) {
	t.Helper()
	lock, _, err := directory.AcquireLock(name, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeCapabilityFile(t *testing.T, directory outputcap.Directory, name string, payload []byte) {
	t.Helper()
	file, err := directory.CreateFile(name, true, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	written, writeErr := file.WriteAt(payload, 0)
	if writeErr == nil && written != len(payload) {
		writeErr = errors.New("short test fixture write")
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close(), directory.Sync()); err != nil {
		t.Fatal(err)
	}
}

func lockLegacyCoordinator(t *testing.T, platform outputcap.Platform, _ legacyFixture) outputcap.Lock {
	t.Helper()
	control, err := platform.Root().OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	lock, created, err := control.AcquireLock(resumestate.CoordinatorLockName, true)
	if err != nil || created {
		t.Fatalf("acquire coordinator fixture: created=%t err=%v", created, err)
	}
	return lock
}

func lockLegacySession(t *testing.T, platform outputcap.Platform, fixture legacyFixture) outputcap.Lock {
	t.Helper()
	control, err := platform.Root().OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	sessions, err := control.OpenDirectory(resumestate.SessionsDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	session, err := sessions.OpenDirectory(filepath.Base(fixture.session), true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	lock, created, err := session.AcquireLock(resumestate.SessionLockName, true)
	if err != nil || created {
		t.Fatalf("acquire session fixture: created=%t err=%v", created, err)
	}
	return lock
}

type changingBindingPlatform struct {
	outputcap.Platform
	mu      sync.Mutex
	calls   int
	failAt  int
	failure error
}

func (platform *changingBindingPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.calls++
	if platform.calls >= platform.failAt {
		return resumestate.OutputRootBinding{}, platform.failure
	}
	return platform.Platform.RootBinding()
}

func runCleanerLockCrashHelper(t *testing.T, rootPath string) {
	t.Helper()
	platform, err := openCleanerTestPlatform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	binding, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := checkpointstore.OpenOwnedNamespace(checkpointstore.NamespaceConfig{
		Root: platform.Root(), BackendID: transfer.NativeFilesystemOutputBackendID,
		RootIdentity: binding.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	lock, _, err := namespace.AcquireLock(FileCheckpointCleanupLock, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "cleaner-lock-acquired")
	for {
		time.Sleep(time.Hour)
	}
}
