//go:build linux

package linuxsubreaper

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/coverage"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/unix"
)

func bestEffortKill(process *os.Process) {
	if process != nil {
		_ = process.Kill()
	}
}

const (
	ownerHelperProcessEnvironment   = "WINDSHARE_OWNER_TEST_HELPER_PROCESS"
	stalledOwnerProcessEnvironment  = "WINDSHARE_OWNER_TEST_STALLED_PROCESS"
	targetModeEnvironment           = "WINDSHARE_OWNER_TEST_TARGET"
	markerPathEnvironment           = "WINDSHARE_OWNER_TEST_MARKER"
	survivorReleasePathEnvironment  = "WINDSHARE_OWNER_TEST_SURVIVOR_RELEASE_PATH"
	targetReadyPathEnvironment      = "WINDSHARE_OWNER_TEST_READY_PATH"
	ownerHelperTestSelection        = "-test.run=TestLinuxOwnerHelperProcess$"
	targetTestSelection             = "-test.run=TestLinuxOwnerTarget$"
	survivorTestSelection           = "-test.run=TestLinuxOwnerSurvivor$"
	execFDFenceCommand              = "exec-fd-fence"
	goTestCoverageDirectoryFlag     = "-test.gocoverdir"
	survivorReleasePathSuffix       = ".release"
	targetReadinessPollInterval     = 10 * time.Millisecond
	survivorReleasePollInterval     = 10 * time.Millisecond
	linuxOwnerBehavioralTestLease   = 10 * time.Second
	linuxOwnerSurvivorResponseLease = 600 * time.Millisecond
)

type ownerHarnessStartMode uint8

const (
	ownerHarnessStartAccepted ownerHarnessStartMode = iota
	ownerHarnessStartRejected
	ownerHarnessStopBeforeAcceptedDecision
)

type ownerHarnessStartResult struct {
	evidence *ownerprotocol.StartEvidence
	err      error
}

func TestMain(testSuite *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == execFDFenceCommand {
		if _, err := unix.FcntlInt(targetExecutableDescriptor, unix.F_GETFD, 0); errors.Is(err, unix.EBADF) {
			os.Exit(0)
		}
		_, _ = fmt.Fprintf(os.Stderr, "authenticated executable fd %d survived native exec\n", targetExecutableDescriptor)
		os.Exit(91)
	}
	if len(os.Args) == 2 && (os.Args[1] == commandSupervise || os.Args[1] == commandExecChild) {
		if os.Args[1] == commandSupervise && os.Getenv(stalledOwnerProcessEnvironment) == "1" {
			runStalledOwnerProcess()
		}
		// Production re-executes /proc/self/exe with unambiguous private commands.
		// Targets use the ordinary test-selection arguments, so these modes bypass
		// the Go harness without affecting the eventual test target.
		coverageDirectory := ""
		if os.Args[1] == commandSupervise {
			coverageDirectory = os.Getenv("GOCOVERDIR")
		}
		exitCode := 0
		if err := runMain(os.Args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, boundedDiagnostic(err))
			exitCode = 1
		}
		if coverageDirectory != "" {
			flushLinuxOwnerHelperCoverage(coverageDirectory)
		}
		os.Exit(exitCode)
	}
	os.Exit(testSuite.Run())
}

func flushLinuxOwnerHelperCoverage(directory string) {
	// Private re-exec modes bypass testing.M, so the test runtime cannot flush
	// their counters. Explicit snapshots keep production-path subprocess evidence
	// in the same coverage corpus without leaking coverage controls to the target.
	_ = coverage.WriteCountersDir(directory)
}

func TestLinuxOwnerFastTargetProvesQuietInventory(t *testing.T) {
	status, _ := runOwnerHarness(t, "fast", 2*time.Second, 100*time.Millisecond, false)
	if status.TreeState != ownerprotocol.TreeProvenEmpty {
		t.Fatalf("fast target status = %#v", status)
	}
	if status.Target.Outcome != ownerprotocol.TargetExited ||
		status.Target.ExitCode == nil || *status.Target.ExitCode != 0 {
		t.Fatalf("fast target evidence = %#v", status.Target)
	}
	if status.Platform.RootStartTimeTicks == "" ||
		status.Platform.QuietInventoryCount != quietInventoryCount {
		t.Fatalf("fast target ownership evidence = %#v", status.Platform)
	}
}

func TestLinuxOwnerReapsImmediateSetsidEscape(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "escaped-survivor-marker")
	status, stdout := runOwnerHarnessWithMarker(
		t,
		"setsid-escape",
		2*time.Second,
		100*time.Millisecond,
		false,
		marker,
	)
	if status.TreeState != ownerprotocol.TreeProvenEmpty || status.Platform.MaximumObservedDescendants < 1 {
		t.Fatalf("setsid escape status = %#v", status)
	}
	match := regexp.MustCompile(`SURVIVOR_PID=([0-9]+)`).FindStringSubmatch(stdout)
	if len(match) != 2 {
		t.Fatalf("setsid survivor PID missing from %q", stdout)
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("setsid survivor pid %d remains visible: %v", pid, err)
	}
	if err := os.WriteFile(
		marker+survivorReleasePathSuffix,
		[]byte("release\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	time.Sleep(linuxOwnerSurvivorResponseLease)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped survivor wrote marker after settlement: %v", err)
	}
}

func TestLinuxOwnerReapsRootFirstMixedDescendantsUnderChurn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "mixed-survivor-marker")
	status, stdout := runOwnerHarnessWithMarker(
		t,
		"root-first-mixed-churn",
		750*time.Millisecond,
		150*time.Millisecond,
		false,
		marker,
	)
	if status.TreeState != ownerprotocol.TreeProvenEmpty ||
		status.TerminationReason != ownerprotocol.TerminationDeadline ||
		status.Platform.MaximumObservedDescendants < 2 {
		t.Fatalf("root-first mixed status = %#v", status)
	}
	for _, label := range []string{"SAME_GROUP_PID", "SETSID_PID"} {
		match := regexp.MustCompile(label + `=([0-9]+)`).FindStringSubmatch(stdout)
		if len(match) != 2 {
			t.Fatalf("%s missing from %q", label, stdout)
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("%s %d remains visible: %v", label, pid, err)
		}
	}
	if err := os.WriteFile(marker+survivorReleasePathSuffix, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(linuxOwnerSurvivorResponseLease)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a mixed survivor wrote after settlement: %v", err)
	}
}

func TestLinuxOwnerDeadlineKillsHangingRoot(t *testing.T) {
	status, _ := runOwnerHarness(t, "hang", time.Second, 100*time.Millisecond, false)
	if status.TreeState != ownerprotocol.TreeProvenEmpty || status.TerminationReason != ownerprotocol.TerminationDeadline {
		t.Fatalf("deadline status = %#v", status)
	}
}

func TestLinuxOwnerParentEOFFenceKillsHangingRoot(t *testing.T) {
	status, _ := runOwnerHarness(t, "hang", 5*time.Second, 100*time.Millisecond, true)
	if status.TreeState != ownerprotocol.TreeProvenEmpty || status.TerminationReason != ownerprotocol.TerminationParentLost {
		t.Fatalf("parent EOF status = %#v", status)
	}
}

func TestLinuxGuardianRetiresStalledOwnerAndAdoptedTree(t *testing.T) {
	t.Setenv(stalledOwnerProcessEnvironment, "1")
	started := time.Now()
	status, stdout := runOwnerHarness(t, "fast", 100*time.Millisecond, 150*time.Millisecond, false)
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("guardian fallback took %s", elapsed)
	}
	if status.TerminationReason != ownerprotocol.TerminationOwnerFailure ||
		status.Target.Outcome != ownerprotocol.TargetStartEvidenceLost ||
		status.TreeState != ownerprotocol.TreeProvenEmpty ||
		status.Cleanup.Outcome != ownerprotocol.CleanupCompleted ||
		status.OwnerFailure == nil || status.OwnerFailure.Code != "GUARDIAN_OWNER_LEASE_EXPIRED" {
		t.Fatalf("guardian fallback settlement = %#v", status)
	}
	for _, label := range []string{"STALLED_OWNER_PID", "SAME_GROUP_PID", "SETSID_PID"} {
		match := regexp.MustCompile(label + `=([0-9]+)`).FindStringSubmatch(stdout)
		if len(match) != 2 {
			t.Fatalf("%s missing from %q", label, stdout)
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			t.Fatalf("%s %d remains visible after guardian fallback: %v", label, pid, err)
		}
	}
}

func TestLinuxOwnerExecutesAuthenticatedScript(t *testing.T) {
	directory := t.TempDir()
	script := filepath.Join(directory, "target.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status := runExecutableOwnerHarness(t, script, []string{}, []ownerprotocol.EnvironmentEntry{}, directory)
	assertSuccessfulTarget(t, status)
}

func TestLinuxOwnerExecutesExecuteOnlyBinary(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "execute-only-target")
	copyExecutable(t, "/proc/self/exe", target)
	if err := os.Chmod(target, 0o111); err != nil {
		t.Fatal(err)
	}
	status := runExecutableOwnerHarness(t, target, []string{execFDFenceCommand}, []ownerprotocol.EnvironmentEntry{}, directory)
	assertSuccessfulTarget(t, status)
}

func TestLinuxOwnerReportsSynchronousExecFailure(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "missing-interpreter.sh")
	if err := os.WriteFile(target, []byte("#!/definitely/missing/interpreter\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status := runExecutableOwnerHarness(t, target, []string{}, []ownerprotocol.EnvironmentEntry{}, directory)
	if status.TerminationReason != ownerprotocol.TerminationInitializationFailed ||
		status.Target.Outcome != ownerprotocol.TargetSpawnFailed ||
		status.Target.FailureCode != "TARGET_EXEC_FAILED" || status.Platform.Root == nil ||
		status.Platform.Root.State != ownerprotocol.RootExited || status.TreeState != ownerprotocol.TreeProvenEmpty {
		t.Fatalf("synchronous exec failure settlement = %#v; root=%+v", status, status.Platform.Root)
	}
}

func TestLinuxOwnerDrainsMaximumInputWhenPreflightPreventsLaunch(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5a}, ownerprotocol.MaximumStdinBytes)
	directory := t.TempDir()
	identity := ownerprotocol.Identity{
		RunID: "linux-owner-tests", OperationID: "linux-owner-preflight-input", Scenario: "invalid-executable",
	}
	request := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable:       filepath.Join(directory, "missing-target"),
		Arguments:        []string{},
		WorkingDirectory: directory,
		Environment:      []ownerprotocol.EnvironmentEntry{},
		Stdin:            &ownerprotocol.Stdin{ByteLength: int64(len(payload))},
	}, 2_000, 150)
	requestBytes, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := runOwnerProtocolHarness(t, requestBytes, payload, false, "")
	if status.TerminationReason != ownerprotocol.TerminationInitializationFailed ||
		status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		status.Target.FailureCode != "EXECUTABLE_INVALID" ||
		status.Input.Outcome != ownerprotocol.InputNotStarted ||
		status.TreeState != ownerprotocol.TreeProvenEmpty ||
		status.Cleanup.Outcome != ownerprotocol.CleanupCompleted ||
		status.OwnerFailure != nil {
		t.Fatalf("preflight input-drain settlement = %#v; owner failure=%+v", status, status.OwnerFailure)
	}
}

func TestLinuxOwnerSettlesLegitimateStartRejectionWithoutExecutingTarget(t *testing.T) {
	status, marker := runOwnerStartBoundaryHarness(t, ownerHarnessStartRejected)
	if status.TerminationReason != ownerprotocol.TerminationStartRejected ||
		status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		status.Target.FailureCode != "START_REJECTED" ||
		status.TreeState != ownerprotocol.TreeProvenEmpty ||
		status.Cleanup.Outcome != ownerprotocol.CleanupCompleted ||
		status.OwnerFailure != nil {
		t.Fatalf("start rejection settlement = %#v", status)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected target executed: %v", err)
	}
}

func TestLinuxOwnerConsumesAcceptedDecisionAfterPreReleaseStop(t *testing.T) {
	status, marker := runOwnerStartBoundaryHarness(t, ownerHarnessStopBeforeAcceptedDecision)
	if status.TerminationReason != ownerprotocol.TerminationStop ||
		status.Target.Outcome != ownerprotocol.TargetNotStarted ||
		status.TreeState != ownerprotocol.TreeProvenEmpty ||
		status.Cleanup.Outcome != ownerprotocol.CleanupCompleted ||
		status.OwnerFailure != nil {
		t.Fatalf("pre-release stop settlement = %#v", status)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped target executed before decision settlement: %v", err)
	}
}

func TestLinuxOwnerRejectsNonPipeMandatoryEventDescriptor(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	request := ownerprotocol.NewRequest(ownerprotocol.Identity{
		RunID: "linux-owner-tests", OperationID: "non-pipe-event", Scenario: "descriptor-authentication",
	}, ownerprotocol.Command{
		Executable: executable, Arguments: []string{}, WorkingDirectory: filepath.Dir(executable),
		Environment: []ownerprotocol.EnvironmentEntry{},
	}, 2_000, 150)
	encoded, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusReader.Close()
	defer statusWriter.Close()
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	inputReader, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputReader.Close()
	defer inputWriter.Close()
	nonPipe, err := os.CreateTemp(t.TempDir(), "event-descriptor-")
	if err != nil {
		t.Fatal(err)
	}
	defer nonPipe.Close()

	owner := exec.Command(executable, ownerHelperArguments(os.Args[1:])...)
	owner.Env = []string{ownerHelperProcessEnvironment + "=1"}
	owner.Stdin = bytes.NewReader(encoded)
	owner.ExtraFiles = []*os.File{statusWriter, controlReader, inputReader, nonPipe}
	output, runErr := owner.CombinedOutput()
	if runErr == nil || !strings.Contains(string(output), "test event descriptor is not an anonymous pipe") {
		t.Fatalf("non-pipe fd6 result = %v; output=%s", runErr, output)
	}
}

func runOwnerStartBoundaryHarness(
	t *testing.T,
	mode ownerHarnessStartMode,
) (ownerprotocol.Settlement, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "target-executed")
	identity := ownerprotocol.Identity{
		RunID: "linux-owner-tests", OperationID: "linux-owner-start-boundary", Scenario: "pre-release",
	}
	request := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable:       executable,
		Arguments:        goTestFixtureArguments([]string{targetTestSelection}),
		WorkingDirectory: filepath.Dir(executable),
		Environment: []ownerprotocol.EnvironmentEntry{
			{Name: markerPathEnvironment, Value: marker},
			{Name: targetModeEnvironment, Value: "launch-marker"},
		},
	}, 2_000, 150)
	requestBytes, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := runOwnerProtocolHarnessWithStartMode(t, requestBytes, nil, false, "", mode)
	return status, marker
}

func TestLinuxOwnerHelperProcess(t *testing.T) {
	if os.Getenv(ownerHelperProcessEnvironment) != "1" {
		return
	}
	if err := runMain([]string{commandGuard}); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxOwnerTarget(t *testing.T) {
	mode := os.Getenv(targetModeEnvironment)
	if mode == "" {
		return
	}
	switch mode {
	case "fast":
		announceTargetReady(t)
		return
	case "hang":
		signal.Ignore(syscall.SIGTERM)
		announceTargetReady(t)
		for {
			time.Sleep(time.Hour)
		}
	case "setsid-escape":
		fmt.Printf("SURVIVOR_PID=%d\n", startLinuxOwnerSurvivor(t, true))
		return
	case "launch-marker":
		if err := os.WriteFile(os.Getenv(markerPathEnvironment), []byte("executed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	case "root-first-mixed-churn":
		sameGroup := startLinuxOwnerSurvivor(t, false)
		newSession := startLinuxOwnerSurvivor(t, true)
		for range 96 {
			child := exec.Command("/bin/sh", "-c", "sleep 0.02")
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			if err := child.Process.Release(); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Printf("SAME_GROUP_PID=%d\nSETSID_PID=%d\n", sameGroup, newSession)
		return
	default:
		t.Fatalf("unknown owner target mode %q", mode)
	}
}

func startLinuxOwnerSurvivor(t *testing.T, newSession bool) int {
	t.Helper()
	child := exec.Command(os.Args[0], goTestFixtureArguments([]string{survivorTestSelection})...)
	child.Env = []string{
		targetModeEnvironment + "=survivor",
		markerPathEnvironment + "=" + os.Getenv(markerPathEnvironment),
		survivorReleasePathEnvironment + "=" + os.Getenv(survivorReleasePathEnvironment),
	}
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if newSession {
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Process.Release(); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestLinuxOwnerSurvivor(t *testing.T) {
	if os.Getenv(targetModeEnvironment) != "survivor" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	releasePath := os.Getenv(survivorReleasePathEnvironment)
	if releasePath == "" || os.Getenv(markerPathEnvironment) == "" {
		t.Fatal("setsid survivor requires release and marker paths")
	}
	for {
		if _, err := os.Lstat(releasePath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(survivorReleasePollInterval)
	}
	if err := os.WriteFile(os.Getenv(markerPathEnvironment), []byte("escaped\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func runStalledOwnerProcess() {
	signal.Ignore(syscall.SIGTERM)
	startSurvivor := func(newSession bool) int {
		command := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do sleep 60; done")
		command.SysProcAttr = &syscall.SysProcAttr{Setsid: newSession}
		if err := command.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(92)
		}
		return command.Process.Pid
	}
	fmt.Printf(
		"STALLED_OWNER_PID=%d\nSAME_GROUP_PID=%d\nSETSID_PID=%d\n",
		os.Getpid(),
		startSurvivor(false),
		startSurvivor(true),
	)
	for {
		time.Sleep(time.Hour)
	}
}

func announceTargetReady(t *testing.T) {
	t.Helper()
	path := os.Getenv(targetReadyPathEnvironment)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runOwnerHarness(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
	closeControl bool,
) (ownerprotocol.Settlement, string) {
	t.Helper()
	return runOwnerHarnessWithMarker(t, mode, deadline, grace, closeControl, "")
}

func runOwnerHarnessWithMarker(
	t *testing.T,
	mode string,
	deadline time.Duration,
	grace time.Duration,
	closeControl bool,
	marker string,
) (ownerprotocol.Settlement, string) {
	t.Helper()
	readyPath := ""
	if closeControl {
		readyPath = filepath.Join(t.TempDir(), "target-ready")
	}
	survivorReleasePath := ""
	if marker != "" {
		survivorReleasePath = marker + survivorReleasePathSuffix
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity := ownerprotocol.Identity{
		RunID: "linux-owner-tests", OperationID: "linux-owner-test-" + strings.ReplaceAll(mode, "-", "_"),
		Scenario: mode,
	}
	request := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable: executable, Arguments: goTestFixtureArguments([]string{targetTestSelection}),
		WorkingDirectory: filepath.Dir(executable),
		Environment: []ownerprotocol.EnvironmentEntry{
			{Name: markerPathEnvironment, Value: marker},
			{Name: targetReadyPathEnvironment, Value: readyPath},
			{Name: survivorReleasePathEnvironment, Value: survivorReleasePath},
			{Name: targetModeEnvironment, Value: mode},
		},
	}, deadline.Milliseconds(), grace.Milliseconds())
	requestBytes, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	return runOwnerProtocolHarness(t, requestBytes, nil, closeControl, readyPath)
}

func runExecutableOwnerHarness(
	t *testing.T,
	executable string,
	arguments []string,
	environment []ownerprotocol.EnvironmentEntry,
	workingDirectory string,
) ownerprotocol.Settlement {
	t.Helper()
	identity := ownerprotocol.Identity{
		RunID: "linux-owner-tests", OperationID: "linux-owner-executable", Scenario: filepath.Base(executable),
	}
	request := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable: executable, Arguments: arguments, WorkingDirectory: workingDirectory, Environment: environment,
	}, 2_000, 150)
	requestBytes, err := ownerprotocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := runOwnerProtocolHarness(t, requestBytes, nil, false, "")
	return status
}

func assertSuccessfulTarget(t *testing.T, status ownerprotocol.Settlement) {
	t.Helper()
	if status.TerminationReason != ownerprotocol.TerminationNatural || status.Target.Outcome != ownerprotocol.TargetExited ||
		status.Target.ExitCode == nil || *status.Target.ExitCode != 0 || status.TreeState != ownerprotocol.TreeProvenEmpty {
		t.Fatalf("successful target settlement = %#v", status)
	}
}

func copyExecutable(t *testing.T, sourcePath, destinationPath string) {
	t.Helper()
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}

func runOwnerProtocolHarness(
	t *testing.T,
	requestBytes []byte,
	rawChildInput []byte,
	closeControl bool,
	readyPath string,
) (ownerprotocol.Settlement, string) {
	t.Helper()
	return runOwnerProtocolHarnessWithStartMode(
		t,
		requestBytes,
		rawChildInput,
		closeControl,
		readyPath,
		ownerHarnessStartAccepted,
	)
}

func runOwnerProtocolHarnessWithStartMode(
	t *testing.T,
	requestBytes []byte,
	rawChildInput []byte,
	closeControl bool,
	readyPath string,
	startMode ownerHarnessStartMode,
) (ownerprotocol.Settlement, string) {
	t.Helper()
	request, err := ownerprotocol.DecodeCanonical[ownerprotocol.Request](requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownerprotocol.ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rawReader, rawWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	startEvidenceReader, startEvidenceWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	startDecisionReader, startDecisionWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(executable, ownerHelperArguments(os.Args[1:])...)
	owner.Env = []string{ownerHelperProcessEnvironment + "=1"}
	if coverageDirectory := os.Getenv("GOCOVERDIR"); coverageDirectory != "" {
		owner.Env = append(owner.Env, "GOCOVERDIR="+coverageDirectory)
	}
	if stalled := os.Getenv(stalledOwnerProcessEnvironment); stalled != "" {
		owner.Env = append(owner.Env, stalledOwnerProcessEnvironment+"="+stalled)
	}
	owner.Stdin = bytes.NewReader(requestBytes)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	owner.Stdout = &stdout
	owner.Stderr = &stderr
	owner.ExtraFiles = []*os.File{
		statusWriter,
		controlReader,
		rawReader,
		eventWriter,
		startEvidenceWriter,
		startDecisionReader,
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bestEffortKill(owner.Process) })
	statusWriter.Close()
	controlReader.Close()
	rawReader.Close()
	eventWriter.Close()
	startEvidenceWriter.Close()
	startDecisionReader.Close()
	eventResult := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(io.Discard, eventReader)
		eventResult <- errors.Join(copyErr, eventReader.Close())
	}()
	startResult := make(chan ownerHarnessStartResult, 1)
	go func() {
		startResult <- completeOwnerHarnessStartGate(
			startEvidenceReader,
			startDecisionWriter,
			controlWriter,
			request,
			startMode,
		)
	}()
	inputResult := make(chan error, 1)
	go func() {
		written, writeErr := rawWriter.Write(rawChildInput)
		closeErr := rawWriter.Close()
		if written != len(rawChildInput) && writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			writeErr = fmt.Errorf(
				"write exact raw child input: wrote %d of %d bytes: %w",
				written,
				len(rawChildInput),
				writeErr,
			)
		}
		inputResult <- errors.Join(writeErr, closeErr)
	}()
	defer controlWriter.Close()
	terminal := make(chan error, 1)
	go func() { terminal <- owner.Wait() }()
	if closeControl {
		waitForTargetReadiness(t, readyPath, terminal, &stderr)
		if err := controlWriter.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-terminal:
		if err != nil {
			t.Fatalf("owner helper failed: %v; stdout=%s; stderr=%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(linuxOwnerBehavioralTestLease):
		t.Fatal("owner helper did not settle within the test lease")
	}
	select {
	case inputErr := <-inputResult:
		if inputErr != nil {
			t.Fatalf("raw child input transport failed: %v; stdout=%s; stderr=%s", inputErr, stdout.String(), stderr.String())
		}
	case <-time.After(maximumInputAbortWait):
		t.Fatal("raw child input transport did not join after owner settlement")
	}
	statusBytes, err := io.ReadAll(statusReader)
	statusReader.Close()
	if err != nil {
		t.Fatal(err)
	}
	status, err := ownerprotocol.DecodeLine[ownerprotocol.Settlement](statusBytes)
	if err != nil {
		t.Fatalf("decode owner status %q: %v", statusBytes, err)
	}
	select {
	case start := <-startResult:
		if start.err != nil {
			t.Fatalf("start gate failed: %v; stdout=%s; stderr=%s", start.err, stdout.String(), stderr.String())
		}
		missingEvidenceIsProvenFailure := status.Target.Outcome == ownerprotocol.TargetSpawnFailed ||
			status.Target.Outcome == ownerprotocol.TargetNotStarted ||
			(status.Target.Outcome == ownerprotocol.TargetStartEvidenceLost &&
				status.TerminationReason == ownerprotocol.TerminationOwnerFailure && status.OwnerFailure != nil)
		if start.evidence == nil && !missingEvidenceIsProvenFailure {
			t.Fatalf("created target omitted start evidence: settlement=%#v", status)
		}
	case <-time.After(linuxOwnerBehavioralTestLease):
		t.Fatal("start gate did not join after owner settlement")
	}
	select {
	case eventErr := <-eventResult:
		if eventErr != nil {
			t.Fatalf("test-event channel failed to close exactly: %v", eventErr)
		}
	case <-time.After(linuxOwnerBehavioralTestLease):
		t.Fatal("test-event channel did not close after owner settlement")
	}
	return status, stdout.String()
}

func completeOwnerHarnessStartGate(
	evidenceReader *os.File,
	decisionWriter *os.File,
	controlWriter *os.File,
	request ownerprotocol.Request,
	mode ownerHarnessStartMode,
) (result ownerHarnessStartResult) {
	defer func() {
		result.err = errors.Join(result.err, evidenceReader.Close(), decisionWriter.Close())
	}()
	reader := bufio.NewReaderSize(evidenceReader, ownerprotocol.MaximumDocumentBytes+4)
	evidence, err := ownerprotocol.ReadFrame[ownerprotocol.StartEvidence](reader)
	if errors.Is(err, io.EOF) {
		return ownerHarnessStartResult{}
	}
	if err != nil {
		return ownerHarnessStartResult{err: fmt.Errorf("read start evidence: %w", err)}
	}
	if trailing, trailingErr := reader.ReadByte(); !errors.Is(trailingErr, io.EOF) || trailing != 0 {
		return ownerHarnessStartResult{err: errors.New("start-evidence frame does not end at exact EOF")}
	}
	if err := validateOwnerHarnessStartEvidence(evidence, request); err != nil {
		return ownerHarnessStartResult{err: err}
	}
	if mode == ownerHarnessStopBeforeAcceptedDecision {
		control := ownerprotocol.Control{
			SchemaVersion: ownerprotocol.ControlSchemaVersion,
			Identity:      request.Identity,
			Reason:        ownerprotocol.ControlReasonStop,
		}
		if err := errors.Join(ownerprotocol.WriteFrame(controlWriter, control), controlWriter.Close()); err != nil {
			return ownerHarnessStartResult{err: fmt.Errorf("publish pre-release stop: %w", err)}
		}
	}
	outcome := ownerprotocol.StartDecisionAccepted
	failureCode := ""
	failureMessage := ""
	if mode == ownerHarnessStartRejected {
		outcome = ownerprotocol.StartDecisionRejected
		failureCode = "TEST_EXECUTABLE_REJECTED"
		failureMessage = "test consumer rejected exact executable identity"
	}
	decision := ownerprotocol.NewStartDecision(evidence, outcome, failureCode, failureMessage)
	if err := ownerprotocol.WriteFrame(decisionWriter, decision); err != nil {
		return ownerHarnessStartResult{err: fmt.Errorf("publish start decision: %w", err)}
	}
	return ownerHarnessStartResult{evidence: &evidence}
}

func validateOwnerHarnessStartEvidence(
	evidence ownerprotocol.StartEvidence,
	request ownerprotocol.Request,
) error {
	if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
		return fmt.Errorf("authenticate request-bound start evidence: %w", err)
	}
	info, err := os.Stat(request.Command.Executable)
	if err != nil {
		return fmt.Errorf("identify requested executable: %w", err)
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("requested executable has no Linux inode identity")
	}
	expected := ownerprotocol.NewObjectIdentity64(uint64(metadata.Dev), metadata.Ino)
	if evidence.Executable != expected {
		return fmt.Errorf("start evidence executable = %#v, want %#v", evidence.Executable, expected)
	}
	return nil
}

func ownerHelperArguments(testArguments []string) []string {
	return goTestFixtureArgumentsFrom(testArguments, []string{ownerHelperTestSelection})
}

func goTestFixtureArguments(arguments []string) []string {
	return goTestFixtureArgumentsFrom(os.Args[1:], arguments)
}

func goTestFixtureArgumentsFrom(testArguments, arguments []string) []string {
	// The outer process owns the final profile. Sharing only its intermediate
	// directory lets the production-path subprocess contribute counters without
	// inheriting unrelated test controls.
	for index := range testArguments {
		argument := testArguments[index]
		if strings.HasPrefix(argument, goTestCoverageDirectoryFlag+"=") {
			return append(arguments, argument)
		}
		if argument == goTestCoverageDirectoryFlag && index+1 < len(testArguments) {
			return append(arguments, argument, testArguments[index+1])
		}
	}
	return arguments
}

func waitForTargetReadiness(
	t *testing.T,
	readyPath string,
	terminal <-chan error,
	stderr *bytes.Buffer,
) {
	t.Helper()
	ticker := time.NewTicker(targetReadinessPollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(linuxOwnerBehavioralTestLease)
	defer timer.Stop()
	for {
		if _, err := os.Lstat(readyPath); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect target readiness: %v", err)
		}
		select {
		case err := <-terminal:
			t.Fatalf("owner helper exited before target readiness: %v; stderr=%s", err, stderr.String())
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("target did not become ready within the test lease")
		}
	}
}
