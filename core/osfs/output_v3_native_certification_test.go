//go:build linux || windows

package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	nativeOutputCertificationProfileEnvironment = "WINDSHARE_REQUIRE_NATIVE_OUTPUT_CERTIFICATION"
	nativeOutputCrashChildProfileEnvironment    = "WINDSHARE_NATIVE_OUTPUT_CRASH_CHILD_PROFILE"
	nativeOutputCrashScenarioEnvironment        = "WINDSHARE_NATIVE_OUTPUT_CRASH_SCENARIO"
	nativeOutputCrashRootEnvironment            = "WINDSHARE_NATIVE_OUTPUT_CRASH_ROOT"
	nativeOutputCrashReadyEnvironment           = "WINDSHARE_NATIVE_OUTPUT_CRASH_READY"
	nativeOutputCrashProbeNameEnvironment       = "WINDSHARE_NATIVE_OUTPUT_CRASH_PROBE_NAME"
	nativeOutputCrashStageSizeEnvironment       = "WINDSHARE_NATIVE_OUTPUT_CRASH_STAGE_SIZE"
	nativeOutputCrashReadyTimeout               = 20 * time.Second
	nativeOutputCrashPollInterval               = 10 * time.Millisecond
	nativeOutputCrashChildMaximumWait           = 5 * time.Minute
	nativeOutputCrashScenarioProbe              = "probe-stage"
)

func openCertifiedNativeOutputForTest(
	t *testing.T,
	root string,
	profile string,
	expectedCertification outputcap.CertificationID,
) outputcap.Platform {
	t.Helper()
	platform, err := openNativeOutputPlatform(root, false)
	if err != nil {
		nativeOutputCertificationFailure(t, profile, "open certified output root", err)
		return nil
	}
	fail := func(operation string, cause error) {
		t.Helper()
		closeErr := platform.Close()
		nativeOutputCertificationFailure(t, profile, operation, errors.Join(cause, closeErr))
	}
	if platform.Certification() != expectedCertification {
		fail("identify native certification", fmt.Errorf(
			"got %q, want %q", platform.Certification(), expectedCertification,
		))
		return nil
	}
	if platform.Durability() != transfer.DurabilityProcessRestart {
		fail("identify native durability", fmt.Errorf(
			"got %v, want process-restart durability", platform.Durability(),
		))
		return nil
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		fail("exercise required native filesystem features", err)
		return nil
	}
	binding, err := platform.RootBinding()
	if err != nil || binding.IsZero() || binding.Certification() != expectedCertification {
		fail("bind certified output root", errors.Join(err, fmt.Errorf(
			"zero=%t certification=%q", binding.IsZero(), binding.Certification(),
		)))
		return nil
	}
	repeated, err := platform.RootBinding()
	if err != nil || repeated != binding {
		fail("repeat certified output-root binding", errors.Join(err, fmt.Errorf(
			"first=%s repeated=%s", binding.String(), repeated.String(),
		)))
		return nil
	}
	return platform
}

func nativeOutputCertificationFailure(t *testing.T, profile, operation string, err error) {
	t.Helper()
	if os.Getenv(nativeOutputCertificationProfileEnvironment) == profile ||
		!errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("%s for required %s profile: %v", operation, profile, err)
	}
	t.Skipf("%s is not certified on this test volume: %v", profile, err)
}

func runNativeOutputProcessRestartRecoveryTest(
	t *testing.T,
	profile string,
	expectedCertification outputcap.CertificationID,
	probeName string,
	stageSize int64,
) {
	t.Helper()
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile &&
		os.Getenv(nativeOutputCrashScenarioEnvironment) == nativeOutputCrashScenarioProbe {
		runNativeOutputCrashChild(t, profile, expectedCertification)
		return
	}
	if os.Getenv(nativeOutputCrashChildProfileEnvironment) == profile {
		return
	}

	base := t.TempDir()
	root := filepath.Join(base, "output")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create native output test root: %v", err)
	}
	preflight := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	if err := preflight.Close(); err != nil {
		t.Fatalf("close native output preflight authority: %v", err)
	}

	readyPath := filepath.Join(base, "child.ready")
	killNativeOutputChildAfterReady(t, readyPath, []string{
		nativeOutputCrashChildProfileEnvironment + "=" + profile,
		nativeOutputCrashScenarioEnvironment + "=" + nativeOutputCrashScenarioProbe,
		nativeOutputCrashRootEnvironment + "=" + root,
		nativeOutputCrashReadyEnvironment + "=" + readyPath,
		nativeOutputCrashProbeNameEnvironment + "=" + probeName,
		nativeOutputCrashStageSizeEnvironment + "=" + strconv.FormatInt(stageSize, 10),
	})

	recovered := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	defer func() {
		if err := recovered.Close(); err != nil {
			t.Errorf("close recovered native output authority: %v", err)
		}
	}()
	if names, err := recovered.Root().Names(0); err != nil || len(names) != 0 {
		t.Fatalf("process-restart recovery left output-root entries %v: %v", names, err)
	}
}

func killNativeOutputChildAfterReady(t *testing.T, readyPath string, environment []string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	// The Linux native chroot intentionally has no device nodes. An EOF-backed
	// pipe keeps os/exec from opening /dev/null before it starts the crash child.
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
	ready := false
	for !ready {
		select {
		case err := <-waited:
			childRunning = false
			t.Fatalf("native output crash child exited before its persisted cut: %v\n%s", err, childOutput.String())
		case <-deadline.C:
			t.Fatalf("native output crash child did not persist its cut within %s\n%s",
				nativeOutputCrashReadyTimeout, childOutput.String())
		case <-ticker.C:
			_, err := os.Stat(readyPath)
			switch {
			case err == nil:
				ready = true
			case errors.Is(err, os.ErrNotExist):
			default:
				t.Fatalf("inspect native output crash-child marker: %v", err)
			}
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill native output crash child: %v", err)
	}
	if err := <-waited; err == nil {
		childRunning = false
		t.Fatal("native output crash child exited successfully instead of being terminated")
	}
	childRunning = false
}

func runNativeOutputCrashChild(
	t *testing.T,
	profile string,
	expectedCertification outputcap.CertificationID,
) {
	t.Helper()
	root := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	probeName := os.Getenv(nativeOutputCrashProbeNameEnvironment)
	stageSize, err := strconv.ParseInt(os.Getenv(nativeOutputCrashStageSizeEnvironment), 10, 64)
	if err != nil || root == "" || readyPath == "" || probeName == "" || stageSize < 0 {
		t.Fatalf("native output crash-child parameters are invalid: %v", err)
	}
	platform := openCertifiedNativeOutputForTest(t, root, profile, expectedCertification)
	directory, err := platform.Root().CreateDirectory(probeName, true)
	if err != nil {
		t.Fatalf("create native probe crash cut: %v", err)
	}
	stage, err := directory.CreateFile("stage", true, stageSize)
	if err != nil {
		t.Fatalf("create native probe stage crash cut: %v", err)
	}
	if err := errors.Join(stage.Sync(), directory.Sync(), platform.Root().Sync()); err != nil {
		t.Fatalf("persist native probe stage crash cut: %v", err)
	}
	signalNativeOutputCrashCut(t, readyPath)
	time.Sleep(nativeOutputCrashChildMaximumWait)
	t.Fatal("native output crash child was not terminated by its parent")
}

func signalNativeOutputCrashCut(t *testing.T, readyPath string) {
	t.Helper()
	// The marker is outside the certified root; it only coordinates the parent
	// process and cannot make an unpersisted output cut appear recoverable.
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("signal native output crash cut: %v", err)
	}
}
