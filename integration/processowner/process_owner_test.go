//go:build windows || linux

// Package processowner contains the real external-process lifecycle oracle.
// Keeping these tests at the integration boundary makes the process fixture
// part of `go test ./integration/...` instead of hiding it in a component unit
// package.
package processowner_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testscenario"
)

const ownedFixtureModeEnvironment = "WINDSHARE_TESTPROCESS_FIXTURE_MODE"

const ownedFixtureOwnerHelperEnvironment = "WINDSHARE_TESTPROCESS_FIXTURE_OWNER_HELPER"

const (
	prebuiltOwnerEnvironment  = "WINDSHARE_TESTPROCESSOWNER_HELPER_PATH"
	repositoryRootEnvironment = "WINDSHARE_TESTPROCESS_REPOSITORY_ROOT"
)

var integrationOwnerFixture struct {
	once  sync.Once
	owner *testprocess.Owner
	err   error
}

func TestMain(suite *testing.M) {
	exitCode := suite.Run()
	if integrationOwnerFixture.owner != nil {
		if err := integrationOwnerFixture.owner.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "close suite process owner: %v\n", err)
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

const (
	ownedFixtureRunEnvironment       = "WINDSHARE_TESTPROCESS_RUN_ID"
	ownedFixtureOperationEnvironment = "WINDSHARE_TESTPROCESS_OPERATION_ID"
	ownedFixtureScenarioEnvironment  = "WINDSHARE_TESTPROCESS_SCENARIO"
)

func TestOwnerStopsExternalFixtureAndProvesTreeEmpty(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/tree-cleanup")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := startFixture(t, trace, owner, repositoryRoot, executable, identity, "tree", nil, 20*time.Second)
	eventContext, cancelEvent := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelEvent()
	event, err := process.Events().Next(eventContext)
	if err != nil {
		t.Fatalf("read fixture event: %v; stdout=%q stderr=%q", err, process.Stdout().String(), process.Stderr().String())
	}
	if event.Identity != identity || event.Component != "owned-fixture" || event.Milestone != "ready" || event.Outcome != "succeeded" {
		t.Fatalf("ready event = %#v", event)
	}
	var payload struct {
		GrandchildPID int `json:"grandchild_pid"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.GrandchildPID < 1 {
		t.Fatalf("ready payload = %s, err=%v", event.Payload, err)
	}
	grandchild, err := retainProcessProbe(payload.GrandchildPID)
	if err != nil {
		t.Fatalf("retain grandchild process identity: %v", err)
	}
	defer grandchild.close()
	stopContext, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	settlement, err := process.Stop(stopContext)
	if err != nil {
		t.Fatalf("stop owned process: %v; stdout=%q stderr=%q", err, process.Stdout().String(), process.Stderr().String())
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	requireStoppedTarget(t, settlement)
	if err := grandchild.waitRetired(2 * time.Second); err != nil {
		t.Fatalf("grandchild remained live after proven-empty settlement: %v", err)
	}
	finishProcessOwnerScenario(t, trace)
}

func TestWaitReturnsNaturalNonzeroExitAsSettlementEvidence(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/nonzero-exit")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := startFixture(t, trace, owner, repositoryRoot, executable, identity, "exit-nonzero", nil, 20*time.Second)
	waitContext, cancelWait := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelWait()
	settlement, err := process.Wait(waitContext)
	if err != nil {
		t.Fatal(err)
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.Target.ExitCode == nil || *settlement.Target.ExitCode != 23 {
		t.Fatalf("exit evidence = %#v", settlement.Target)
	}
	if err := testprocess.RequireSuccess(settlement, nil); err == nil {
		t.Fatal("RequireSuccess accepted target exit code 23")
	}
	finishProcessOwnerScenario(t, trace)
}

func TestOwnerDeliversExactStdinThroughPrivateChannel(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/exact-stdin")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("private-process-owner-input")
	process := startFixture(t, trace, owner, repositoryRoot, executable, identity, "stdin", input, 20*time.Second)
	waitContext, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	settlement, err := process.Wait(waitContext)
	if err != nil {
		t.Fatalf("wait for stdin fixture: %v; stdout=%q stderr=%q", err, process.Stdout().String(), process.Stderr().String())
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.Input.Outcome != protocol.InputDelivered || !bytes.Contains(process.Stdout().Bytes, input) {
		t.Fatalf("stdin settlement = %#v; stdout=%q stderr=%q", settlement.Input, process.Stdout().String(), process.Stderr().String())
	}
	finishProcessOwnerScenario(t, trace)
}

func TestOwnerDeadlineRetiresWholeTree(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/tree-deadline")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := startFixture(t, trace, owner, repositoryRoot, executable, identity, "tree", nil, 750*time.Millisecond)
	waitContext, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWait()
	settlement, err := process.Wait(waitContext)
	if err != nil {
		t.Fatal(err)
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.TerminationReason != protocol.TerminationDeadline {
		t.Fatalf("deadline settlement = %#v", settlement)
	}
	finishProcessOwnerScenario(t, trace)
}

func TestOwnerSurvivesClientDeathAndRetiresWholeTree(t *testing.T) {
	trace, identity := startProcessOwnerScenario(t, "integration/processowner/client-death")
	repositoryRoot := testRepositoryRoot(t)
	owner := integrationOwner(t, repositoryRoot)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment, err := testprocess.InheritEnvironment(map[string]string{
		ownedFixtureModeEnvironment:        "owner-parent",
		ownedFixtureRunEnvironment:         identity.RunID,
		ownedFixtureOperationEnvironment:   identity.OperationID,
		ownedFixtureScenarioEnvironment:    identity.Scenario,
		ownedFixtureOwnerHelperEnvironment: owner.HelperExecutable(),
		repositoryRootEnvironment:          repositoryRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(executable, "-test.run=^TestOwnedProcessFixture$", "-test.count=1")
	parent.Env = environmentStrings(environment)
	parent.Dir = repositoryRoot
	parentOutput, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	parentErrors := newMilestoneWriter()
	parent.Stderr = parentErrors
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	harness := newParentDeathHarness(parent)
	trace.RequireCleanup(t, "client-death harness", harness.cleanup)

	payloadResult := make(chan struct {
		payload parentDeathPayload
		err     error
	}, 1)
	go func() {
		var payload parentDeathPayload
		line, readErr := bufio.NewReader(parentOutput).ReadBytes('\n')
		if readErr == nil {
			readErr = json.Unmarshal(bytes.TrimSpace(line), &payload)
		}
		payloadResult <- struct {
			payload parentDeathPayload
			err     error
		}{payload: payload, err: readErr}
	}()
	var payload parentDeathPayload
	select {
	case result := <-payloadResult:
		if result.err != nil {
			t.Fatalf("read client-death fixture: %v; stderr=%q", result.err, parentErrors.String())
		}
		payload = result.payload
	case <-time.After(10 * time.Second):
		t.Fatalf("client-death fixture did not become ready; stderr=%q", parentErrors.String())
	}
	if payload.RootPID < 1 || payload.GrandchildPID < 1 {
		t.Fatalf("client-death payload = %#v", payload)
	}
	if err := harness.retainTree(payload); err != nil {
		t.Fatal(err)
	}
	if err := harness.killClientAndWait(10 * time.Second); err != nil {
		t.Fatalf("retire client process: %v; stderr=%q", err, parentErrors.String())
	}
	if err := harness.waitTreeRetired(10 * time.Second); err != nil {
		t.Fatalf("owner did not retire tree after client death: %v", err)
	}
	finishProcessOwnerScenario(t, trace)
}

func startFixture(
	t *testing.T,
	trace *testscenario.Trace,
	owner *testprocess.Owner,
	repositoryRoot, executable string,
	identity protocol.Identity,
	mode string,
	input []byte,
	deadline time.Duration,
) *testprocess.Process {
	t.Helper()
	environment, err := ownedFixtureEnvironment(identity, mode)
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), testprocess.Spec{
		Identity: identity,
		Command: testprocess.Command{
			Executable: executable,
			// The selection enters a child-only fixture mode inside this already
			// selected integration scenario; it is not suite ownership or scheduling.
			Arguments:        []string{"-test.run=^TestOwnedProcessFixture$", "-test.count=1"},
			WorkingDirectory: repositoryRoot,
			Environment:      environment,
			Stdin:            input,
		},
		Deadline:         deadline,
		TerminationGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace.RequireCleanup(t, "owned process tree", func(cleanupContext context.Context) error {
		_, cleanupErr := process.StopAndRequireTreeEmpty(cleanupContext)
		return cleanupErr
	})
	return process
}

func TestOwnedProcessFixture(t *testing.T) {
	mode := os.Getenv(ownedFixtureModeEnvironment)
	if mode != "tree" && mode != "grandchild" && mode != "exit-nonzero" && mode != "stdin" &&
		mode != "verbose-output" && mode != "owner-parent" {
		t.Skip("invoked only as an externally owned process fixture")
	}
	if mode == "owner-parent" {
		runOwnerParentFixture(t)
		return
	}
	if mode == "grandchild" {
		time.Sleep(time.Minute)
		return
	}
	_, _ = os.Stdout.WriteString("fixture-ready\n")
	if mode == "tree" {
		command := exec.Command(os.Args[0], "-test.run=^TestOwnedProcessFixture$", "-test.count=1")
		grandchildEnvironment, err := ownedFixtureEnvironment(ownedFixtureIdentity(), "grandchild")
		if err != nil {
			t.Fatal(err)
		}
		command.Env = make([]string, 0, len(grandchildEnvironment))
		for _, entry := range grandchildEnvironment {
			command.Env = append(command.Env, entry.Name+"="+entry.Value)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		grandchildPID := command.Process.Pid
		if err := command.Process.Release(); err != nil {
			t.Fatal(err)
		}
		sink, err := testprocess.OpenEventSink(ownedFixtureIdentity())
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Emit("owned-fixture", "ready", "succeeded", map[string]any{
			"pid": os.Getpid(), "grandchild_pid": grandchildPID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "exit-nonzero" {
		os.Exit(23)
	}
	if mode == "stdin" {
		input, err := io.ReadAll(io.LimitReader(os.Stdin, protocol.MaximumStdinBytes+1))
		if err != nil || len(input) > protocol.MaximumStdinBytes {
			t.Fatalf("read bounded fixture stdin: bytes=%d err=%v", len(input), err)
		}
		if _, err := os.Stdout.Write(input); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "verbose-output" {
		chunk := bytes.Repeat([]byte{0x6f}, 32<<10)
		remaining := testprocess.MaximumCapturedOutputBytes + 1
		for remaining > 0 {
			next := min(remaining, len(chunk))
			written, err := os.Stdout.Write(chunk[:next])
			if err != nil || written != next {
				t.Fatalf("write output-capture fixture: bytes=%d err=%v", written, err)
			}
			remaining -= next
		}
		return
	}
	time.Sleep(time.Minute)
}

func runOwnerParentFixture(t *testing.T) {
	owner, err := testprocess.NewOwner(os.Getenv(ownedFixtureOwnerHelperEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	identity := ownedFixtureIdentity()
	environment, err := ownedFixtureEnvironment(identity, "tree")
	if err != nil {
		t.Fatal(err)
	}
	process, err := owner.Start(context.Background(), testprocess.Spec{
		Identity: identity,
		Command: testprocess.Command{
			Executable: executable, Arguments: []string{"-test.run=^TestOwnedProcessFixture$", "-test.count=1"},
			WorkingDirectory: os.Getenv(repositoryRootEnvironment), Environment: environment,
		},
		Deadline: time.Minute, TerminationGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventContext, cancelEvent := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelEvent()
	event, err := process.Events().Next(eventContext)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		RootPID       int `json:"pid"`
		GrandchildPID int `json:"grandchild_pid"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(parentDeathPayload{
		RootPID: payload.RootPID, GrandchildPID: payload.GrandchildPID,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func ownedFixtureEnvironment(identity protocol.Identity, mode string) ([]protocol.EnvironmentEntry, error) {
	return testprocess.InheritEnvironment(map[string]string{
		ownedFixtureModeEnvironment:      mode,
		ownedFixtureRunEnvironment:       identity.RunID,
		ownedFixtureOperationEnvironment: identity.OperationID,
		ownedFixtureScenarioEnvironment:  identity.Scenario,
	})
}

func environmentStrings(environment []protocol.EnvironmentEntry) []string {
	values := make([]string, 0, len(environment))
	for _, entry := range environment {
		values = append(values, entry.Name+"="+entry.Value)
	}
	return values
}

type parentDeathPayload struct {
	RootPID       int `json:"root_pid"`
	GrandchildPID int `json:"grandchild_pid"`
}

type parentDeathHarness struct {
	command *exec.Cmd
	done    chan struct{}

	mu      sync.Mutex
	waitErr error
	probes  []*processProbe
}

func newParentDeathHarness(command *exec.Cmd) *parentDeathHarness {
	harness := &parentDeathHarness{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		harness.mu.Lock()
		harness.waitErr = err
		harness.mu.Unlock()
		close(harness.done)
	}()
	return harness
}

func (harness *parentDeathHarness) retainTree(payload parentDeathPayload) error {
	root, err := retainProcessProbe(payload.RootPID)
	if err != nil {
		return fmt.Errorf("retain root process identity: %w", err)
	}
	harness.probes = []*processProbe{root}
	grandchild, err := retainProcessProbe(payload.GrandchildPID)
	if err != nil {
		return fmt.Errorf("retain grandchild process identity: %w", err)
	}
	harness.probes = append(harness.probes, grandchild)
	return nil
}

func (harness *parentDeathHarness) killClientAndWait(maximum time.Duration) error {
	select {
	case <-harness.done:
	default:
		if err := harness.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	select {
	case <-harness.done:
		harness.mu.Lock()
		waitErr := harness.waitErr
		harness.mu.Unlock()
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) {
			return waitErr
		}
		return nil
	case <-timer.C:
		return errors.New("client process did not stop within its bounded join")
	}
}

func (harness *parentDeathHarness) waitTreeRetired(maximum time.Duration) error {
	deadline := time.Now().Add(maximum)
	var result error
	for _, probe := range harness.probes {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			result = errors.Join(result, errors.New("process-tree probes exceeded their shared retirement deadline"))
			break
		}
		result = errors.Join(result, probe.waitRetired(remaining))
	}
	return result
}

func (harness *parentDeathHarness) cleanup(cleanupContext context.Context) error {
	result := harness.killClientAndWait(cleanupDuration(cleanupContext, 10*time.Second))
	result = errors.Join(
		result,
		harness.waitTreeRetired(cleanupDuration(cleanupContext, 10*time.Second)),
	)
	for _, probe := range harness.probes {
		probe.close()
	}
	harness.probes = nil
	return result
}

func cleanupDuration(cleanupContext context.Context, maximum time.Duration) time.Duration {
	if err := cleanupContext.Err(); err != nil {
		return 0
	}
	deadline, bounded := cleanupContext.Deadline()
	if !bounded {
		return maximum
	}
	remaining := time.Until(deadline)
	if remaining < maximum {
		return max(remaining, 0)
	}
	return maximum
}

func ownedFixtureIdentity() protocol.Identity {
	return protocol.Identity{
		RunID:       os.Getenv(ownedFixtureRunEnvironment),
		OperationID: os.Getenv(ownedFixtureOperationEnvironment),
		Scenario:    os.Getenv(ownedFixtureScenarioEnvironment),
	}
}

func testRepositoryRoot(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv(repositoryRootEnvironment); configured != "" {
		if !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
			t.Fatalf("configured repository root is not absolute and canonical: %q", configured)
		}
		return configured
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for current := filepath.Clean(workingDirectory); ; current = filepath.Dir(current) {
		if fileExists(filepath.Join(current, "go.work")) &&
			fileExists(filepath.Join(current, "go.mod")) &&
			fileExists(filepath.Join(current, "core", "go.mod")) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	t.Fatal("repository root was not found from the test working directory")
	return ""
}

func fileExists(path string) bool {
	information, err := os.Stat(path)
	return err == nil && information.Mode().IsRegular()
}

func integrationOwner(t *testing.T, repositoryRoot string) *testprocess.Owner {
	t.Helper()
	integrationOwnerFixture.once.Do(func() {
		if helperPath := os.Getenv(prebuiltOwnerEnvironment); helperPath != "" {
			integrationOwnerFixture.owner, integrationOwnerFixture.err = testprocess.NewOwner(helperPath)
			return
		}
		buildContext, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
		defer cancelBuild()
		integrationOwnerFixture.owner, integrationOwnerFixture.err = testprocess.BuildOwner(
			buildContext,
			repositoryRoot,
		)
	})
	if integrationOwnerFixture.err != nil {
		t.Fatalf("initialize suite process owner: %v", integrationOwnerFixture.err)
	}
	return integrationOwnerFixture.owner
}

type milestoneWriter struct {
	mu      sync.Mutex
	content bytes.Buffer
}

func newMilestoneWriter() *milestoneWriter { return &milestoneWriter{} }

func (writer *milestoneWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.content.Write(value)
}

func (writer *milestoneWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.content.String()
}
