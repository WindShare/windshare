//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ownerHelperProcessEnvironment   = "WINDSHARE_OWNER_TEST_HELPER_PROCESS"
	targetModeEnvironment           = "WINDSHARE_OWNER_TEST_TARGET"
	markerPathEnvironment           = "WINDSHARE_OWNER_TEST_MARKER"
	survivorReleasePathEnvironment  = "WINDSHARE_OWNER_TEST_SURVIVOR_RELEASE_PATH"
	targetReadyPathEnvironment      = "WINDSHARE_OWNER_TEST_READY_PATH"
	ownerHelperTestSelection        = "-test.run=TestLinuxOwnerHelperProcess$"
	targetTestSelection             = "-test.run=TestLinuxOwnerTarget$"
	survivorTestSelection           = "-test.run=TestLinuxOwnerSurvivor$"
	goTestCoverageDirectoryFlag     = "-test.gocoverdir"
	survivorReleasePathSuffix       = ".release"
	targetReadinessPollInterval     = 10 * time.Millisecond
	survivorReleasePollInterval     = 10 * time.Millisecond
	linuxOwnerBehavioralTestLease   = 10 * time.Second
	linuxOwnerSurvivorResponseLease = 600 * time.Millisecond
)

func TestMain(testSuite *testing.M) {
	if os.Getenv(ownerHelperProcessEnvironment) == "1" &&
		len(os.Args) == 2 && os.Args[1] == commandExecChild {
		// Production re-executes /proc/self/exe for the authenticated exec gate.
		// The target's canonical environment strips this sentinel, so only the gate
		// bypasses the Go harness instead of recursively starting the whole suite.
		if err := runMain(os.Args[1:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, boundedDiagnostic(err))
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testSuite.Run())
}

func TestLinuxOwnerFastTargetProvesQuietInventory(t *testing.T) {
	status, _ := runOwnerHarness(t, "fast", 2*time.Second, 100*time.Millisecond, false)
	if !status.Launched || !status.TreeEmpty || status.TimedOut {
		t.Fatalf("fast target status = %#v", status)
	}
	if status.ProcessEvidence.Terminal != "exited" ||
		status.ProcessEvidence.ExitCode == nil || *status.ProcessEvidence.ExitCode != 0 {
		t.Fatalf("fast target process evidence = %#v", status.ProcessEvidence)
	}
	if status.OwnershipEvidence.RootStartTimeTicks == "" ||
		status.OwnershipEvidence.QuietInventoryCount != quietInventoryCount {
		t.Fatalf("fast target ownership evidence = %#v", status.OwnershipEvidence)
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
	if !status.TreeEmpty || status.OwnershipEvidence.MaximumObservedDescendants < 1 {
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

func TestLinuxOwnerDeadlineKillsHangingRoot(t *testing.T) {
	status, _ := runOwnerHarness(t, "hang", 100*time.Millisecond, 100*time.Millisecond, false)
	if !status.Launched || !status.TreeEmpty || !status.TimedOut ||
		status.OwnershipEvidence.ControlOutcome != "deadline" {
		t.Fatalf("deadline status = %#v", status)
	}
}

func TestLinuxOwnerParentEOFFenceKillsHangingRoot(t *testing.T) {
	status, _ := runOwnerHarness(t, "hang", 5*time.Second, 100*time.Millisecond, true)
	if !status.Launched || !status.TreeEmpty || status.TimedOut ||
		status.OwnershipEvidence.ControlOutcome != "parent-eof" {
		t.Fatalf("parent EOF status = %#v", status)
	}
}

func TestLinuxOwnerHelperProcess(t *testing.T) {
	if os.Getenv(ownerHelperProcessEnvironment) != "1" {
		return
	}
	if err := runMain([]string{commandRun}); err != nil {
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
		child := exec.Command(os.Args[0], survivorTestSelection)
		child.Env = []string{
			targetModeEnvironment + "=survivor",
			markerPathEnvironment + "=" + os.Getenv(markerPathEnvironment),
			survivorReleasePathEnvironment + "=" + os.Getenv(survivorReleasePathEnvironment),
		}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("SURVIVOR_PID=%d\n", child.Process.Pid)
		return
	default:
		t.Fatalf("unknown owner target mode %q", mode)
	}
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
) (ownerStatus, string) {
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
) (ownerStatus, string) {
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
	metadata, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	executableDigest := hex.EncodeToString(digest.Sum(nil))
	executableByteLength := metadata.Size()
	request := ownerRequest{
		SchemaVersion: requestSchemaVersion,
		OperationID:   "linux-owner-test-" + strings.ReplaceAll(mode, "-", "_"),
		Command: commandRequest{
			Executable:           executable,
			ExecutableSHA256:     &executableDigest,
			ExecutableByteLength: &executableByteLength,
			Arguments:            []string{targetTestSelection},
			CWD:                  filepath.Dir(executable),
			Environment: map[string]string{
				targetModeEnvironment:          mode,
				markerPathEnvironment:          marker,
				survivorReleasePathEnvironment: survivorReleasePath,
				targetReadyPathEnvironment:     readyPath,
			},
			Stdin: nil,
		},
		DeadlineMS:         deadline.Milliseconds(),
		TerminationGraceMS: grace.Milliseconds(),
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return runOwnerProtocolHarness(t, requestBytes, nil, closeControl, readyPath)
}

func runOwnerProtocolHarness(
	t *testing.T,
	requestBytes []byte,
	rawChildInput []byte,
	closeControl bool,
	readyPath string,
) (ownerStatus, string) {
	t.Helper()
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
	owner := exec.Command(executable, ownerHelperArguments(os.Args[1:])...)
	owner.Env = []string{ownerHelperProcessEnvironment + "=1"}
	owner.Stdin = bytes.NewReader(requestBytes)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	owner.Stdout = &stdout
	owner.Stderr = &stderr
	owner.ExtraFiles = []*os.File{statusWriter, controlReader, rawReader}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bestEffortKill(owner.Process) })
	statusWriter.Close()
	controlReader.Close()
	rawReader.Close()
	if _, err := rawWriter.Write(rawChildInput); err != nil {
		rawWriter.Close()
		t.Fatal(err)
	}
	if err := rawWriter.Close(); err != nil {
		t.Fatal(err)
	}
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
			t.Fatalf("owner helper failed: %v; stderr=%s", err, stderr.String())
		}
	case <-time.After(linuxOwnerBehavioralTestLease):
		t.Fatal("owner helper did not settle within the test lease")
	}
	statusBytes, err := io.ReadAll(statusReader)
	statusReader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var status ownerStatus
	if err := json.Unmarshal(bytes.TrimSpace(statusBytes), &status); err != nil {
		t.Fatalf("decode owner status %q: %v", statusBytes, err)
	}
	return status, stdout.String()
}

func ownerHelperArguments(testArguments []string) []string {
	arguments := []string{ownerHelperTestSelection}
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
