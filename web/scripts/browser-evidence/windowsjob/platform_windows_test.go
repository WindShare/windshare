//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/windshare/windshare/internal/testnetwork"
)

const (
	testHelperEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_HELPER"
	testTargetEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_TARGET"
	testMarkerEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_MARKER"
	testReadyEnvironment         = "WINDSHARE_WINDOWSJOB_TEST_READY"
	testCWDEnvironment           = "WINDSHARE_WINDOWSJOB_TEST_CWD"
	testParentEnvironment        = "WINDSHARE_WINDOWSJOB_TEST_PARENT"
	testParentRequestEnvironment = "WINDSHARE_WINDOWSJOB_TEST_PARENT_REQUEST"
	testParentStatusEnvironment  = "WINDSHARE_WINDOWSJOB_TEST_PARENT_STATUS"
	testParentControlEnvironment = "WINDSHARE_WINDOWSJOB_TEST_PARENT_CONTROL"
	rootNaturalExitCode          = 37
	rootBeforeDescendantExitCode = 7
	launcherReleaseRootExitCode  = 46
)

const (
	testMarkerPollInterval         = 20 * time.Millisecond
	testMarkerWaitLimit            = 10 * time.Second
	rootReadyDelay                 = 300 * time.Millisecond
	launcherReleaseRootDelay       = 750 * time.Millisecond
	naturalDescendantDelay         = 250 * time.Millisecond
	deadlineTestMS           int64 = 3_000
	deadlineDescendantDelay        = 5 * time.Second
	postDeadlineObservation        = 3 * time.Second
	breakawayWriterDelay           = 1 * time.Second
	postBreakawayObservation       = 1_500 * time.Millisecond
	crashDescendantDelay           = 2 * time.Second
	postCrashObservation           = 3 * time.Second
)

func TestMain(testMain *testing.M) {
	if os.Getenv(testParentEnvironment) == "1" {
		os.Exit(runWindowsParentFixture())
	}
	if target := os.Getenv(testTargetEnvironment); target != "" {
		os.Exit(runWindowsTargetFixture(target))
	}
	if os.Getenv(testHelperEnvironment) == "1" {
		if err := runCommand(os.Args[1:], os.Stdin); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testMain.Run())
}

func TestWindowsSupervisorPreservesCommandAndStreams(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Arguments = []string{"plain", "", `quote"slash\`, "<>&\u2028"}
	result := runWindowsSupervisorProcess(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	wantStdout := "stdout\x00fake-status-frame\n" + strings.Join(request.Arguments, "\x1f")
	if result.stdout != wantStdout {
		t.Fatalf("stdout = %q, want %q", result.stdout, wantStdout)
	}
	if result.stderr != "stderr<>&\u2028" {
		t.Fatalf("stderr = %q", result.stderr)
	}
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, rootNaturalExitCode)
}

func TestWindowsSupervisorDeliversExactRawStdinAfterContainment(t *testing.T) {
	rawInput := []byte{0, 1, 2, 0xff, '\n'}
	request := windowsIntegrationRequest(t, "stdin", 5_000)
	request.Stdin = &rawStdin{
		Kind:       rawStdinKind,
		Descriptor: 0,
		ByteLength: int64(len(rawInput)),
		ChannelID:  "channel",
		RunID:      "run",
		ProfileID:  "profile",
		AttemptID:  "attempt",
	}
	result := runWindowsSupervisorProcess(t, request, rawInput)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if !bytes.Equal([]byte(result.stdout), rawInput) || result.stderr != "" {
		t.Fatalf("raw stdin delivery = stdout %v, stderr %q", []byte(result.stdout), result.stderr)
	}
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, rootNaturalExitCode)
}

func TestWindowsSupervisorReportsExactStillActiveExitCode(t *testing.T) {
	request := windowsIntegrationRequest(t, "exit-259", 5_000)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, 259)
}

func TestWindowsSupervisorWaitsForNaturalDescendantDrain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "natural-marker")
	request := windowsIntegrationRequest(t, "natural-descendant", 5_000)
	request.Environment = targetEnvironment("natural-descendant", marker, request.CWD)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if encoded, err := os.ReadFile(marker); err != nil || string(encoded) != "natural-descendant" {
		t.Fatalf("natural descendant marker = %q, err = %v", encoded, err)
	}
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, rootBeforeDescendantExitCode)
}

func TestWindowsSupervisorKillsLingeringDescendantAtDeadline(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late-marker")
	ready := filepath.Join(t.TempDir(), "descendant-ready")
	request := windowsIntegrationRequest(t, "deadline-descendant", deadlineTestMS)
	request.Environment = targetEnvironment("deadline-descendant", marker, request.CWD)
	request.Environment = upsertEnvironmentEntry(request.Environment, testReadyEnvironment, ready)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if encoded, err := os.ReadFile(ready); err != nil || string(encoded) != "descendant-started" {
		t.Fatalf("deadline descendant readiness = %q, err = %v", encoded, err)
	}
	assertTreeStatus(t, result.status, request, terminationReasonDeadline, true, rootBeforeDescendantExitCode)
	time.Sleep(postDeadlineObservation)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminated descendant wrote after authoritative status: %v", err)
	}
}

func TestWindowsSupervisorRejectsBreakawayDescendant(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "breakaway-marker")
	request := windowsIntegrationRequest(t, "breakaway-attempt", 5_000)
	request.Environment = targetEnvironment("breakaway-attempt", marker, request.CWD)
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(t, result.status, request, terminationReasonNatural, false, 0)
	time.Sleep(postBreakawayObservation)
	encoded, err := os.ReadFile(marker)
	if err != nil || string(encoded) != "breakaway-blocked" {
		t.Fatalf("breakaway marker = %q, err = %v", encoded, err)
	}
}

func TestWindowsSupervisorCrashClosesTheJobLease(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	marker := filepath.Join(t.TempDir(), "crash-late-marker")
	ready := filepath.Join(t.TempDir(), "crash-ready")
	statusPath := filepath.Join(t.TempDir(), "status.json")
	request := windowsIntegrationRequest(t, "crash-tree", 15_000)
	request.Environment = targetEnvironment("crash-tree", marker, request.CWD)
	request.Environment = upsertEnvironmentEntry(request.Environment, testReadyEnvironment, ready)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(t.TempDir(), "request.bin")
	controlPath := filepath.Join(filepath.Dir(requestPath), "control.bin")
	writeCanonicalRequestFile(t, requestPath, request)
	command := exec.Command(
		executable,
		commandSupervise,
		"--status", statusPath,
		"--request", requestPath,
		"--control", controlPath,
	)
	replacements := map[string]string{testHelperEnvironment: "1"}
	if coverageDirectory != "" {
		replacements["GOCOVERDIR"] = coverageDirectory
	}
	command.Env = replaceEnvironment(os.Environ(), replacements)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := waitForMarker(ready, nil); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if encoded, err := os.ReadFile(ready); err != nil || string(encoded) != "tree-ready" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("crash tree readiness = %q, err = %v", encoded, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if waitErr := command.Wait(); waitErr == nil {
		t.Fatal("abruptly killed supervisor reported success")
	}
	_ = input.Close()

	time.Sleep(postCrashObservation)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived supervisor Job Object lease closure: %v", err)
	}
	if _, err := os.Stat(statusPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed supervisor published authority status: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("crash fixture wrote target streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWindowsSupervisorPublishesSpawnFailureAfterEmptyJob(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	request.Executable = filepath.Join(t.TempDir(), "does-not-exist.exe")
	result := runWindowsSupervisor(t, request, nil)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	if result.status.SupervisionOutcome != statusOutcomeSpawnFailed || result.status.Root != nil || result.status.SpawnFailure == nil {
		t.Fatalf("spawn failure status = %#v", result.status)
	}
	if len(*result.status.SpawnFailure) == 0 || len(*result.status.SpawnFailure) > maximumDiagnosticBytes {
		t.Fatalf("spawn failure diagnostic length = %d", len(*result.status.SpawnFailure))
	}
	if result.status.ActiveProcessCount != 0 || result.status.TerminationReason != terminationReasonTargetSpawnFailed {
		t.Fatalf("spawn failure authority = %#v", result.status)
	}
}

func TestWindowsSupervisorParentRequestTerminatesRoot(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "root-ready")
	request := windowsIntegrationRequest(t, "long-root", 5_000)
	request.Environment = targetEnvironment("long-root", ready, request.CWD)
	result := runWindowsSupervisorThroughExpiringParent(t, request)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(
		t,
		result.status,
		request,
		terminateReasonParentRequest,
		false,
		mustTerminationExitCodes(t, request.Nonce).parent,
	)
}

func TestWindowsSupervisorAuthenticatesCreateNewControlRequest(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "root-ready")
	request := windowsIntegrationRequest(t, "long-root", 5_000)
	request.Environment = targetEnvironment("long-root", ready, request.CWD)
	result := runWindowsSupervisorWithControl(t, request)
	if result.err != nil {
		t.Fatalf("supervisor: %v", result.err)
	}
	assertTreeStatus(
		t,
		result.status,
		request,
		terminateReasonParentRequest,
		false,
		mustTerminationExitCodes(t, request.Nonce).parent,
	)
}

func TestTerminationExitCodesArePrivateDeterministicAndDistinct(t *testing.T) {
	first := mustTerminationExitCodes(t, testNonce)
	if repeated := mustTerminationExitCodes(t, testNonce); repeated != first {
		t.Fatalf("same nonce produced different termination codes: %#v then %#v", first, repeated)
	}
	alternate := mustTerminationExitCodes(t, strings.Repeat("b", nonceEncodedBytes))
	if alternate == first {
		t.Fatalf("different nonces produced identical termination code sets: %#v", first)
	}
	seen := make(map[uint32]struct{}, 3)
	for reason, code := range map[string]uint32{
		"deadline":  first.deadline,
		"parent":    first.parent,
		"authority": first.authority,
	} {
		if code == 0 || code == windowsStillActiveExitCode {
			t.Fatalf("%s termination code is ambiguous: %d", reason, code)
		}
		if code&uint32(windows.APPLICATION_ERROR) == 0 {
			t.Fatalf("%s termination code lacks APPLICATION_ERROR class: %#x", reason, code)
		}
		if _, duplicate := seen[code]; duplicate {
			t.Fatalf("termination code is reused across reasons: %#x", code)
		}
		seen[code] = struct{}{}
	}
}

func mustTerminationExitCodes(t *testing.T, nonce string) terminationExitCodes {
	t.Helper()
	codes, err := deriveTerminationExitCodes(nonce)
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func TestWindowsSupervisorStatusIsNoClobber(t *testing.T) {
	request := windowsIntegrationRequest(t, "echo", 5_000)
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status.json")
	const sentinel = "user-owned"
	if err := os.WriteFile(statusPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runWindowsSupervisorAt(t, request, statusPath, nil)
	if result.err == nil {
		t.Fatal("supervisor accepted preexisting status path")
	}
	encoded, err := os.ReadFile(statusPath)
	if err != nil || string(encoded) != sentinel {
		t.Fatalf("preexisting status changed: %q, %v", encoded, err)
	}
}

func TestWindowsEnvironmentUsesOrdinalIgnoreCaseUniqueness(t *testing.T) {
	equal, err := windowsEnvironmentNamesEqual("Ä_NAME", "ä_name")
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatal("CompareStringOrdinal did not identify non-ASCII case pair")
	}
	if err := validateWindowsEnvironment([]environmentEntry{{Name: "Ä_NAME"}, {Name: "ä_name"}}); err == nil {
		t.Fatal("Windows-case-insensitive duplicate environment was accepted")
	}
}

type windowsSupervisorResult struct {
	status supervisorStatus
	stdout string
	stderr string
	err    error
}

func runWindowsSupervisor(t *testing.T, request startRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	return runWindowsSupervisorAt(t, request, filepath.Join(t.TempDir(), "status.json"), rawInput)
}

func runWindowsSupervisorAt(t *testing.T, request startRequest, statusPath string, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	controlPath := filepath.Join(filepath.Dir(statusPath), "control.bin")
	rawReader, rawWrite := exactRawInputReader(t, request, rawInput)
	runErr := runSupervisorPlatform(request, statusPath, controlPath, rawReader)
	if closeErr := rawReader.Close(); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if rawWrite != nil {
		if writeErr := <-rawWrite; runErr == nil && writeErr != nil {
			runErr = writeErr
		}
	}
	return decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{err: runErr})
}

func runWindowsSupervisorProcess(t *testing.T, request startRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	return runWindowsSupervisorProcessAt(t, request, filepath.Join(t.TempDir(), "status.json"), rawInput)
}

func runWindowsSupervisorProcessAt(t *testing.T, request startRequest, statusPath string, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	requestPath := filepath.Join(filepath.Dir(statusPath), "request.bin")
	controlPath := filepath.Join(filepath.Dir(statusPath), "control.bin")
	writeCanonicalRequestFile(t, requestPath, request)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		executable,
		commandSupervise,
		"--status", statusPath,
		"--request", requestPath,
		"--control", controlPath,
	)
	replacements := map[string]string{testHelperEnvironment: "1"}
	if coverageDirectory != "" {
		replacements["GOCOVERDIR"] = coverageDirectory
	}
	command.Env = replaceEnvironment(os.Environ(), replacements)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := writeAndEraseRawInput(input, request, rawInput); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	waitErr := command.Wait()
	result := windowsSupervisorResult{stdout: stdout.String(), stderr: stderr.String(), err: waitErr}
	return decodeWindowsSupervisorResult(t, statusPath, result)
}

func exactRawInputReader(
	t *testing.T,
	request startRequest,
	rawInput []byte,
) (*os.File, <-chan error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if request.Stdin == nil {
		if len(rawInput) != 0 {
			_ = reader.Close()
			_ = writer.Close()
			t.Fatal("test supplied undeclared raw stdin")
		}
		if err := writer.Close(); err != nil {
			_ = reader.Close()
			t.Fatal(err)
		}
		return reader, nil
	}
	if int64(len(rawInput)) != request.Stdin.ByteLength {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("test raw stdin length = %d, declared %d", len(rawInput), request.Stdin.ByteLength)
	}
	payload := bytes.Clone(rawInput)
	result := make(chan error, 1)
	go func() {
		defer func() {
			for index := range payload {
				payload[index] = 0
			}
		}()
		result <- errors.Join(writeAll(writer, payload), writer.Close())
	}()
	return reader, result
}

func writeAndEraseRawInput(writer io.WriteCloser, request startRequest, rawInput []byte) error {
	payload := bytes.Clone(rawInput)
	defer func() {
		for index := range payload {
			payload[index] = 0
		}
	}()
	if request.Stdin == nil {
		if len(payload) != 0 {
			_ = writer.Close()
			return errors.New("test supplied undeclared raw stdin")
		}
		return writer.Close()
	}
	if int64(len(payload)) != request.Stdin.ByteLength {
		_ = writer.Close()
		return fmt.Errorf("test raw stdin length = %d, declared %d", len(payload), request.Stdin.ByteLength)
	}
	return errors.Join(writeAll(writer, payload), writer.Close())
}

func writeCanonicalRequestFile(t *testing.T, path string, request startRequest) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := writeCanonicalFrame(file, request)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func runWindowsSupervisorThroughExpiringParent(
	t *testing.T,
	request startRequest,
) windowsSupervisorResult {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status.json")
	requestPath := filepath.Join(directory, "request.bin")
	controlPath := filepath.Join(directory, "control.bin")
	writeCanonicalRequestFile(t, requestPath, request)
	ready := environmentValue(request.Environment, testMarkerEnvironment)
	if ready == "" {
		t.Fatal("parent-loss request has no readiness marker")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	parentAuthority, err := testnetwork.NewOSNetworkChildAuthority(executable, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := parentAuthority.Retire(); err != nil {
			t.Errorf("retire parent-fixture OS-network authority: %v", err)
		}
	})
	authorityName, authorityValue := parentAuthority.EnvironmentVariable()
	replacements := map[string]string{
		testParentEnvironment:        "1",
		testParentRequestEnvironment: requestPath,
		testParentStatusEnvironment:  statusPath,
		testParentControlEnvironment: controlPath,
		testMarkerEnvironment:        ready,
		authorityName:                authorityValue,
	}
	if coverageDirectory != "" {
		replacements["GOCOVERDIR"] = coverageDirectory
	}
	command.Env = replaceEnvironment(os.Environ(), replacements)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Run(); err != nil {
		return windowsSupervisorResult{err: fmt.Errorf("expiring parent fixture: %w", err)}
	}
	if err := waitForStatusPublication(statusPath); err != nil {
		return windowsSupervisorResult{err: err}
	}
	return decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{})
}

func runWindowsSupervisorWithControl(t *testing.T, request startRequest) windowsSupervisorResult {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	directory := t.TempDir()
	statusPath := filepath.Join(directory, "status.json")
	controlPath := filepath.Join(directory, "control.bin")
	ready := environmentValue(request.Environment, testMarkerEnvironment)
	if ready == "" {
		t.Fatal("control request has no readiness marker")
	}
	controlResult := make(chan error, 1)
	go func() {
		if err := waitForMarker(ready, nil); err != nil {
			controlResult <- err
			return
		}
		controlResult <- publishTerminationControl(controlPath, terminateRequest{
			SchemaVersion: protocolSchemaVersion,
			Type:          requestTypeTerminate,
			OperationID:   request.OperationID,
			Nonce:         request.Nonce,
			Reason:        terminateReasonParentRequest,
		})
	}()
	rawReader, _ := exactRawInputReader(t, request, nil)
	runErr := runSupervisorPlatform(request, statusPath, controlPath, rawReader)
	_ = rawReader.Close()
	if controlErr := <-controlResult; runErr == nil && controlErr != nil {
		runErr = controlErr
	}
	return decodeWindowsSupervisorResult(t, statusPath, windowsSupervisorResult{err: runErr})
}

func publishTerminationControl(path string, control terminateRequest) error {
	staged, err := os.CreateTemp(filepath.Dir(path), ".control-*.tmp")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	if err := writeCanonicalFrame(staged, control); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Link(stagedPath, path)
}

func runWindowsParentFixture() int {
	testnetwork.AssertOSNetwork()
	requestPath := os.Getenv(testParentRequestEnvironment)
	statusPath := os.Getenv(testParentStatusEnvironment)
	controlPath := os.Getenv(testParentControlEnvironment)
	readyPath := os.Getenv(testMarkerEnvironment)
	if requestPath == "" || statusPath == "" || controlPath == "" || readyPath == "" {
		return 111
	}
	executable, err := os.Executable()
	if err != nil {
		return 112
	}
	command := exec.Command(
		executable,
		commandSupervise,
		"--status", statusPath,
		"--request", requestPath,
		"--control", controlPath,
	)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testHelperEnvironment: "1",
		testParentEnvironment: "",
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	input, err := command.StdinPipe()
	if err != nil {
		return 113
	}
	if err := command.Start(); err != nil {
		return 114
	}
	if err := input.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return 115
	}
	if err := waitForMarker(readyPath, nil); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return 116
	}
	if err := command.Process.Release(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return 117
	}
	return 0
}

func waitForStatusPublication(path string) error {
	deadline := time.NewTimer(testMarkerWaitLimit)
	defer deadline.Stop()
	poll := time.NewTicker(testMarkerPollInterval)
	defer poll.Stop()
	for {
		encoded, err := os.ReadFile(path)
		if err == nil && json.Valid(encoded) {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read status publication: %w", err)
		}
		select {
		case <-deadline.C:
			return errors.New("authority status was not published in time")
		case <-poll.C:
		}
	}
}

func decodeWindowsSupervisorResult(t *testing.T, statusPath string, result windowsSupervisorResult) windowsSupervisorResult {
	t.Helper()
	if result.err != nil {
		return result
	}
	encoded, readErr := os.ReadFile(statusPath)
	if readErr != nil {
		return result
	}
	if bytes.HasSuffix(encoded, []byte{'\n'}) {
		t.Fatal("status has trailing LF")
	}
	if err := json.Unmarshal(encoded, &result.status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	canonical, err := canonicalJSON(result.status)
	if err != nil || !bytes.Equal(canonical, encoded) {
		t.Fatalf("status is not canonical: %v", err)
	}
	return result
}

func prepareChildProcessEnvironment(t *testing.T, request startRequest) (startRequest, string) {
	t.Helper()
	coverageDirectory := ""
	if testing.CoverMode() != "" {
		coverageDirectory = t.TempDir()
		request.Environment = upsertEnvironmentEntry(request.Environment, "GOCOVERDIR", coverageDirectory)
	}
	if _, err := os.Stat(request.Executable); err != nil {
		// Spawn-failure tests deliberately name an absent executable. Test-only
		// delegation must not preempt the production failure and its diagnostics.
		if errors.Is(err, os.ErrNotExist) {
			return request, coverageDirectory
		}
		t.Fatalf("inspect target before OS-network delegation: %v", err)
	}
	authority, err := testnetwork.NewOSNetworkChildAuthority(request.Executable, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Retire(); err != nil {
			t.Errorf("retire target OS-network authority: %v", err)
		}
	})
	authorityName, authorityValue := authority.EnvironmentVariable()
	request.Environment = upsertEnvironmentEntry(request.Environment, authorityName, authorityValue)
	return request, coverageDirectory
}

func waitForMarker(path string, finished <-chan struct{}) error {
	deadline := time.NewTimer(testMarkerWaitLimit)
	defer deadline.Stop()
	poll := time.NewTicker(testMarkerPollInterval)
	defer poll.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect readiness marker: %w", err)
		}
		select {
		case <-finished:
			return errors.New("supervisor finished before target readiness marker")
		case <-deadline.C:
			return errors.New("target readiness marker was not published in time")
		case <-poll.C:
		}
	}
}

func windowsIntegrationRequest(t *testing.T, target string, deadlineMS int64) startRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Clean(t.TempDir())
	return startRequest{
		SchemaVersion:      protocolSchemaVersion,
		Type:               requestTypeStart,
		OperationID:        "windows-integration-" + target,
		Nonce:              testNonce,
		Executable:         filepath.Clean(executable),
		Arguments:          []string{},
		CWD:                cwd,
		Environment:        targetEnvironment(target, "", cwd),
		DeadlineMS:         deadlineMS,
		TerminationGraceMS: 3_000,
	}
}

func targetEnvironment(target, marker, cwd string) []environmentEntry {
	values := map[string]string{
		testTargetEnvironment: target,
		testCWDEnvironment:    cwd,
	}
	if marker != "" {
		values[testMarkerEnvironment] = marker
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		values["SystemRoot"] = systemRoot
	}
	environment := make([]environmentEntry, 0, len(values))
	for name, value := range values {
		environment = append(environment, environmentEntry{Name: name, Value: value})
	}
	sort.Slice(environment, func(left, right int) bool {
		return compareEnvironmentNames(environment[left].Name, environment[right].Name) < 0
	})
	return environment
}

func upsertEnvironmentEntry(environment []environmentEntry, name, value string) []environmentEntry {
	result := make([]environmentEntry, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			if !replaced {
				result = append(result, environmentEntry{Name: name, Value: value})
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, environmentEntry{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool {
		return compareEnvironmentNames(result[left].Name, result[right].Name) < 0
	})
	return result
}

func environmentValue(environment []environmentEntry, name string) string {
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			return entry.Value
		}
	}
	return ""
}

func assertTreeStatus(t *testing.T, status supervisorStatus, request startRequest, reason string, timedOut bool, exitCode uint32) {
	t.Helper()
	if status.SchemaVersion != protocolSchemaVersion || status.OperationID != request.OperationID || status.Nonce != request.Nonce {
		t.Fatalf("status identity = %#v", status)
	}
	if status.SupervisionOutcome != statusOutcomeTreeEmpty || status.TerminationReason != reason || status.TimedOut != timedOut {
		t.Fatalf("status outcome = %#v", status)
	}
	if status.ActiveProcessCount != 0 || status.Root == nil || status.Root.ExitCode != exitCode || status.SpawnFailure != nil {
		t.Fatalf("status authority = %#v", status)
	}
	if status.InputOutcome != settledInputOutcome(request) {
		t.Fatalf("status input outcome = %q, want %q", status.InputOutcome, settledInputOutcome(request))
	}
}

func runWindowsTargetFixture(mode string) int {
	switch mode {
	case "echo":
		cwd, err := os.Getwd()
		if err != nil || filepath.Clean(cwd) != filepath.Clean(os.Getenv(testCWDEnvironment)) {
			return 91
		}
		_, _ = os.Stdout.Write([]byte("stdout\x00fake-status-frame\n" + strings.Join(os.Args[1:], "\x1f")))
		_, _ = os.Stderr.Write([]byte("stderr<>&\u2028"))
		return rootNaturalExitCode
	case "stdin":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 103
		}
		defer func() {
			for index := range input {
				input[index] = 0
			}
		}()
		if _, err := os.Stdout.Write(input); err != nil {
			return 104
		}
		return rootNaturalExitCode
	case "exit-259":
		windows.ExitProcess(259)
		return 259
	case "launcher-release-root":
		time.Sleep(launcherReleaseRootDelay)
		return launcherReleaseRootExitCode
	case "natural-descendant":
		if err := startLateWriter(naturalDescendantDelay); err != nil {
			return 92
		}
		return rootBeforeDescendantExitCode
	case "deadline-descendant":
		if err := startLateWriter(deadlineDescendantDelay); err != nil {
			return 93
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("descendant-started"), 0o600); err != nil {
			return 99
		}
		return rootBeforeDescendantExitCode
	case "crash-tree":
		if err := startLateWriter(crashDescendantDelay); err != nil {
			return 101
		}
		if err := os.WriteFile(os.Getenv(testReadyEnvironment), []byte("tree-ready"), 0o600); err != nil {
			return 102
		}
		time.Sleep(10 * time.Second)
		return 0
	case "late-writer":
		delay, err := time.ParseDuration(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_DELAY"))
		if err != nil {
			return 94
		}
		time.Sleep(delay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte(os.Getenv("WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE")), 0o600); err != nil {
			return 95
		}
		return 0
	case "breakaway-attempt":
		if err := startBreakawayWriter(); err == nil {
			return 96
		}
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("breakaway-blocked"), 0o600); err != nil {
			return 97
		}
		return 0
	case "long-root":
		time.Sleep(rootReadyDelay)
		if err := os.WriteFile(os.Getenv(testMarkerEnvironment), []byte("root-ready"), 0o600); err != nil {
			return 100
		}
		time.Sleep(10 * time.Second)
		return 0
	default:
		return 98
	}
}

func startLateWriter(delay time.Duration) error {
	testnetwork.AssertOSNetwork()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        delay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "natural-descendant",
	})
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func startBreakawayWriter() error {
	testnetwork.AssertOSNetwork()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		testTargetEnvironment:                    "late-writer",
		"WINDSHARE_WINDOWSJOB_TEST_DELAY":        breakawayWriterDelay.String(),
		"WINDSHARE_WINDOWSJOB_TEST_MARKER_VALUE": "escaped",
	})
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, assignment := range environment {
		separator := strings.IndexByte(assignment, '=')
		if separator < 1 {
			continue
		}
		name := assignment[:separator]
		replaced := false
		for replacement := range replacements {
			if strings.EqualFold(name, replacement) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, assignment)
		}
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}
