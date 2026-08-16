package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testprocess"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	v2ProcessTimeout              = 30 * time.Second
	v2DurableResumeProcessTimeout = 90 * time.Second
	v2OwnedProcessDeadline        = 5 * time.Minute
	v2ProcessTerminationGrace     = 10 * time.Second
)

const (
	v2ResumeFileCount        = 2
	v2ResumeFileBytes        = 8 << 20
	v2FailureLogStreamBytes  = 4 << 10
	v2OutputControlDirectory = ".windshare-output"

	v2PionRelayCutPayloadBytes  int64 = 1 << 20
	v2PionRelayCutBlockBytes          = 4 << 10
	v2PionRelayCutPayloadSHA256       = "fbbab289f7f94b25736c58be46a994c441fd02552cc6022352e3d86d2fab7c83"

	v2RelayComponent          = "wsrelay"
	v2WindShareShareComponent = "windshare_share"
	v2WindShareGetComponent   = "windshare_get"
)

const (
	v2ProgressiveCatalogScenario = "v2-progressive-catalog"
	v2PionRelayCutScenario       = "v2-pion-relay-cut"
	v2DurableResumeScenario      = "v2-durable-resume"
)

type processOutputView struct {
	snapshot func() testprocess.OutputSnapshot
}

func (view *processOutputView) String() string {
	if view == nil || view.snapshot == nil {
		return ""
	}
	return view.snapshot().String()
}

func (view *processOutputView) diagnosticString() string {
	value := view.String()
	if len(value) <= v2FailureLogStreamBytes {
		return value
	}
	// Keep both causal startup output and the terminal tail without letting a
	// many-file cascade turn one failed assertion into an unbounded CI log.
	edgeBytes := v2FailureLogStreamBytes / 2
	omitted := len(value) - 2*edgeBytes
	return fmt.Sprintf("%s\n... %d bytes omitted ...\n%s", value[:edgeBytes], omitted, value[len(value)-edgeBytes:])
}

type v2Process struct {
	scenario  *v2Scenario
	component string
	owner     *testprocess.Owner
	owned     *testprocess.Process
	stdout    *processOutputView
	stderr    *processOutputView
	done      chan struct{}
	result    testprocess.Result
	err       error
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error

	userTracePath      string
	userTraceCommand   string
	traceForbidden     []string
	stderrForbidden    []string
	userTraceValidated bool
}

func startV2Process(
	t *testing.T,
	scenario *v2Scenario,
	component string,
	binary string,
	arguments ...string,
) *v2Process {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: component}
	phase := scenario.startPhase(t, v2ProcessStartMilestone, phaseContext)
	environment, err := testprocess.InheritEnvironment(nil)
	if err != nil {
		t.Fatalf("prepare %s child environment: %v", component, err)
	}
	binaries := loadE2EBinaries(t)
	owner, err := testprocess.NewOwner(binaries.processOwner)
	if err != nil {
		t.Fatalf("open external process owner for %s: %v", component, err)
	}
	process := &v2Process{
		scenario:  scenario,
		component: component,
		owner:     owner,
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
	}
	cleanupName := fmt.Sprintf("%s process tree", component)
	t.Cleanup(func() {
		if err := process.close(); err != nil {
			t.Errorf(
				"clean externally owned process tree: run_id=%s operation_id=%s scenario=%s component=%s cleanup=%v",
				scenario.operation.RunID(), scenario.operation.ID(), scenario.operation.Scenario(), component, err,
			)
		}
	})
	scenario.trace.RequireCleanup(t, cleanupName, process.closeWithContext)
	process.owned, err = owner.Start(t.Context(), testprocess.Spec{
		Identity: testrun.Identity{
			RunID:       scenario.operation.RunID(),
			OperationID: scenario.operation.ID(),
			Scenario:    scenario.operation.Scenario(),
		},
		Command: testprocess.Command{
			Executable: binary, Arguments: arguments, WorkingDirectory: repoRoot(), Environment: environment,
		},
		Deadline: v2OwnedProcessDeadline, TerminationGrace: v2ProcessTerminationGrace,
	})
	if err != nil {
		t.Fatalf("start externally owned %s (%s): %v", component, filepath.Base(binary), err)
	}
	process.stdout = &processOutputView{snapshot: process.owned.Stdout}
	process.stderr = &processOutputView{snapshot: process.owned.Stderr}
	go func() {
		process.result, process.err = process.owned.Wait(context.Background())
		close(process.done)
	}()
	scenario.succeedPhase(t, phase, phaseContext)
	return process
}

func startTracedV2Process(
	t *testing.T,
	scenario *v2Scenario,
	component string,
	binary string,
	arguments ...string,
) *v2Process {
	t.Helper()
	command := ""
	if len(arguments) > 0 {
		command = arguments[0]
	}
	if command != "share" && command != "get" {
		t.Fatalf("user trace requested for unsupported command %q", command)
	}
	tracePath := filepath.Join(t.TempDir(), component+"-user-trace.ndjson")
	if !filepath.IsAbs(tracePath) {
		t.Fatalf("user trace path is not absolute: %q", tracePath)
	}
	arguments = append(append([]string(nil), arguments...), "--trace", tracePath)
	process := startV2Process(t, scenario, component, binary, arguments...)
	process.userTracePath = tracePath
	process.userTraceCommand = command
	return process
}

func requireV2ProcessScenario(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("real child processes and end-to-end transfer exceed the short-test budget")
	}
}

func (process *v2Process) close() error {
	if process == nil {
		return nil
	}
	closeContext, cancel := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
	defer cancel()
	return process.closeWithContext(closeContext)
}

func (process *v2Process) closeWithContext(closeContext context.Context) error {
	if process == nil {
		return nil
	}
	if closeContext == nil {
		return errors.New("close process context is nil")
	}
	process.closeOnce.Do(func() {
		go process.runClose(closeContext)
	})
	select {
	case <-process.closeDone:
		return process.closeErr
	case <-closeContext.Done():
		return closeContext.Err()
	}
}

func (process *v2Process) runClose(closeContext context.Context) {
	defer close(process.closeDone)
	var stopErr error
	var cleanupErr error
	if process.owned != nil {
		result, err := process.owned.Stop(closeContext)
		stopErr = err
		if result.ExitCode != nil {
			cleanupErr = testprocess.RequireClean(result, nil)
		}
	}
	var ownerErr error
	if process.owner != nil {
		ownerErr = process.owner.Close()
	}
	process.closeErr = errors.Join(stopErr, cleanupErr, ownerErr)
}

func (process *v2Process) stop(t *testing.T) {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessStopMilestone, phaseContext)
	if err := process.close(); err != nil {
		recordErr := phase.Fail(v2ActionFailureReason)
		t.Fatalf(
			"stop externally owned process tree: run_id=%s operation_id=%s scenario=%s component=%s cleanup=%v stdout=%q stderr=%q",
			process.scenario.operation.RunID(), process.scenario.operation.ID(), process.scenario.operation.Scenario(), process.component,
			errors.Join(err, recordErr), process.stdout.diagnosticString(), process.stderr.diagnosticString(),
		)
	}
	process.scenario.succeedPhase(t, phase, phaseContext)
}

func (process *v2Process) interrupt(t *testing.T) {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessStopMilestone, phaseContext)
	interruptContext, cancel := context.WithTimeout(t.Context(), v2ProcessTerminationGrace)
	defer cancel()
	result, err := process.owned.Interrupt(interruptContext)
	if cleanErr := testprocess.RequireSuccess(result, err); cleanErr != nil {
		recordErr := phase.Fail(v2ActionFailureReason)
		t.Fatalf(
			"interrupt externally owned process tree: run_id=%s operation_id=%s scenario=%s component=%s result=%+v error=%v stdout=%q stderr=%q",
			process.scenario.operation.RunID(), process.scenario.operation.ID(), process.scenario.operation.Scenario(), process.component,
			result, errors.Join(cleanErr, recordErr), process.stdout.diagnosticString(), process.stderr.diagnosticString(),
		)
	}
	process.scenario.succeedPhase(t, phase, phaseContext)
}

func (process *v2Process) wait(t *testing.T) error {
	return process.waitWithin(t, v2ProcessTimeout)
}

func (process *v2Process) waitWithin(t *testing.T, timeout time.Duration) error {
	t.Helper()
	result, err := process.waitResultWithin(t, timeout)
	return testprocess.RequireSuccess(result, err)
}

func (process *v2Process) waitResultWithin(
	t *testing.T,
	timeout time.Duration,
) (testprocess.Result, error) {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessWaitMilestone, phaseContext)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
		process.validateUserTrace(t)
		if process.err != nil {
			return process.result, errors.Join(process.err, phase.Fail(v2ProcessWaitFailureReason))
		}
		return process.result, phase.Succeed(phaseContext)
	case <-timer.C:
		cleanupErr := process.close()
		t.Fatalf(
			"process timeout: run_id=%s operation_id=%s scenario=%s component=%s cleanup=%v stdout=%q stderr=%q",
			process.scenario.operation.RunID(), process.scenario.operation.ID(), process.scenario.operation.Scenario(), process.component,
			cleanupErr, process.stdout.diagnosticString(), process.stderr.diagnosticString(),
		)
		return testprocess.Result{}, context.DeadlineExceeded
	}
}

func waitV2Match(t *testing.T, process *v2Process, expression *regexp.Regexp, stream *processOutputView) string {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessReadinessMilestone, phaseContext)
	readyContext, cancel := context.WithTimeout(t.Context(), v2ProcessTimeout)
	defer cancel()
	outputStream := testprocess.Stdout
	if stream == process.stderr {
		outputStream = testprocess.Stderr
	}
	match, err := process.owned.WaitForOutput(readyContext, outputStream, expression)
	if err != nil {
		t.Fatalf("process readiness failed: %v", err)
	}
	process.scenario.succeedPhase(t, phase, phaseContext)
	return match[1]
}

func waitV2RelayReady(t *testing.T, process *v2Process) string {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessReadinessMilestone, phaseContext)
	if process.component != v2RelayComponent {
		t.Fatalf("relay readiness requested from component %q", process.component)
	}
	readyContext, cancel := context.WithTimeout(t.Context(), v2ProcessTimeout)
	defer cancel()
	event, err := process.owned.Events().Next(readyContext)
	if err != nil {
		t.Fatalf(
			"read private relay readiness event: run_id=%s operation_id=%s scenario=%s err=%v stdout=%q stderr=%q",
			process.scenario.operation.RunID(), process.scenario.operation.ID(), process.scenario.operation.Scenario(), err,
			process.stdout.diagnosticString(), process.stderr.diagnosticString(),
		)
	}
	if event.Component != v2RelayComponent || event.Milestone != testrun.ListenerReadyMilestone ||
		event.Outcome != string(testrun.OutcomeSucceeded) {
		t.Fatalf("unexpected private relay readiness event: %#v", event)
	}
	var payload testrun.ListenerReadyContext
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode private relay readiness payload: %v", err)
	}
	if err := validateV2RelayAddress(payload.Address); err != nil {
		t.Fatal(err)
	}
	process.scenario.succeedPhase(t, phase, phaseContext)
	return payload.Address
}

func validateV2RelayAddress(address string) error {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listener-ready address %q: %w", address, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65_535 {
		return fmt.Errorf("listener-ready address %q has invalid port", address)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listener-ready address %q is not loopback", address)
	}
	return nil
}

func TestLongV2ProcessProgressiveCatalogConcurrentReceiversAndSelection(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2ProgressiveCatalogScenario)
	binaries := loadE2EBinaries(t)
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(root, "nested", "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stable.txt"), []byte("root-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.txt"), []byte("selected-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zero.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	relayState := filepath.Join(t.TempDir(), "relay-state")
	relay := startV2Process(
		t, scenario, v2RelayComponent, binaries.relay,
		"-listen", "127.0.0.1:0", "-state-dir", relayState,
	)
	address := waitV2RelayReady(t, relay)
	relayURL := "ws://" + address
	share := startV2Process(t, scenario, v2WindShareShareComponent, binaries.windshare, "share", root, "--relay", relayURL)
	linkExpression := regexp.MustCompile(`(?m)^Link: (\S+)$`)
	shareLink := waitV2Match(t, share, linkExpression, share.stdout)
	capabilitySecrets := v2CapabilityForbiddenValues(shareLink)
	outputs := []string{
		testoutputroot.New(t).RootPath,
		testoutputroot.New(t).RootPath,
	}
	receivers := make([]*v2Process, 0, len(outputs))
	for _, output := range outputs {
		receivers = append(receivers, startV2Process(
			t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
		))
	}
	for index, receiver := range receivers {
		if err := receiver.wait(t); err != nil {
			t.Fatalf("receiver %d failed: %v; stdout=%q stderr=%q", index, err, receiver.stdout.String(), receiver.stderr.String())
		}
		assertV2File(t, filepath.Join(outputs[index], "tree", "stable.txt"), []byte("root-content"))
		assertV2File(t, filepath.Join(outputs[index], "tree", "nested", "a.txt"), []byte("selected-content"))
		assertV2File(t, filepath.Join(outputs[index], "tree", "zero.bin"), nil)
		if info, err := os.Stat(filepath.Join(outputs[index], "tree", "nested", "empty-dir")); err != nil || !info.IsDir() {
			t.Fatalf("receiver %d empty directory: info=%v err=%v", index, info, err)
		}
		assertV2OutputInventory(t, outputs[index], map[string]bool{
			"tree":                  true,
			"tree/nested":           true,
			"tree/nested/empty-dir": true,
			"tree/nested/a.txt":     false,
			"tree/stable.txt":       false,
			"tree/zero.bin":         false,
		})
	}

	selectedOutput := testoutputroot.New(t).RootPath
	selected := startTracedV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare,
		"get", shareLink, "-o", selectedOutput, "--only", "tree/nested/a.txt",
	)
	selected.forbidStderr(capabilitySecrets...)
	selected.forbidUserTrace(append(capabilitySecrets, root, "tree/nested/a.txt", selectedOutput)...)
	if err := selected.wait(t); err != nil {
		t.Fatalf("selected receiver failed: %v; stdout=%q stderr=%q", err, selected.stdout.String(), selected.stderr.String())
	}
	requireV2UserTraceFact(t, selected, "transfer_settled", "result_status", "success")
	assertV2File(t, filepath.Join(selectedOutput, "a.txt"), []byte("selected-content"))
	assertV2OutputInventory(t, selectedOutput, map[string]bool{
		"a.txt": false,
	})
	scenario.requireSuccess(t)
}

func TestLongV2ProcessTransfersExactPayloadOverPionAfterRelayCut(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2PionRelayCutScenario)
	binaries := loadE2EBinaries(t)
	source := filepath.Join(t.TempDir(), "pion-relay-cut.bin")
	writeV2PatternFile(t, source, v2PionRelayCutPayloadBytes, v2PionRelayCutPayloadSHA256)

	proxy := startRelayCutProxy(t, scenario)
	relay := startV2Process(
		t,
		scenario,
		v2RelayComponent,
		binaries.relay,
		"-listen", "127.0.0.1:0",
		"-relay-base-url", proxy.BaseURL(),
		"-state-dir", filepath.Join(t.TempDir(), "relay-state"),
	)
	relayAddress := waitV2RelayReady(t, relay)
	if err := scenario.observe(
		v2RelayProxyForwardMilestone,
		nil,
		func() error { return proxy.ForwardTo(relayAddress) },
	); err != nil {
		t.Fatal(err)
	}

	share := startV2Process(
		t,
		scenario,
		v2WindShareShareComponent,
		binaries.windshare,
		"share", source,
		"--relay", proxy.BaseURL(),
		"--block-size", fmt.Sprint(v2PionRelayCutBlockBytes),
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	capabilitySecrets := v2CapabilityForbiddenValues(shareLink)
	output := testoutputroot.New(t).RootPath
	receiver := startTracedV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
	receiver.forbidStderr(capabilitySecrets...)
	receiver.forbidUserTrace(append(capabilitySecrets, source, filepath.Base(source), output)...)
	// A data channel becomes useful only after both runtimes own the attached lane.
	// Waiting for both private milestones prevents relay loss from racing the
	// sender-side adoption that the receiver cannot observe, without exposing a
	// private correctness cut through user-facing output or trace.
	waitV2ProcessTrace(t, receiver, v2ReceiverDirectLaneMilestone, testrun.OutcomeSucceeded)
	waitV2ProcessTrace(t, share, v2SenderDirectLaneMilestone, testrun.OutcomeSucceeded)
	select {
	case <-receiver.done:
		t.Fatalf(
			"receiver completed before relay cut; stdout=%q stderr=%q",
			receiver.stdout.String(),
			receiver.stderr.String(),
		)
	default:
	}
	cutPhase := scenario.startPhase(t, v2RelayProxyCutMilestone, nil)
	cutContext, cancelCut := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
	relayDownstream, proxyErr := proxy.CutAndWait(cutContext)
	cancelCut()
	if proxyErr != nil {
		t.Fatalf("cut relay proxy: %v", errors.Join(proxyErr, cutPhase.Fail(v2ActionFailureReason)))
	}
	scenario.succeedPhase(t, cutPhase, nil)
	relay.stop(t)
	if relayDownstream == 0 {
		t.Fatal("relay cut observed no bootstrap or signaling traffic")
	}
	if err := receiver.wait(t); err != nil {
		t.Fatalf(
			"receiver failed after authenticated Pion activation and relay cut: %v; stdout=%q stderr=%q",
			err,
			receiver.stdout.String(),
			receiver.stderr.String(),
		)
	}
	requireV2UserTraceFact(t, receiver, "content_path_selected", "content_path", "direct")

	outputPath := filepath.Join(output, filepath.Base(source))
	assertV2FileSHA256(
		t,
		outputPath,
		v2PionRelayCutPayloadBytes,
		v2PionRelayCutPayloadSHA256,
	)
	scenario.requireSuccess(t)
}

func writeV2PatternFile(t *testing.T, filename string, size int64, wantSHA256 string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	pattern := make([]byte, v2PionRelayCutBlockBytes)
	for index := range pattern {
		pattern[index] = byte(index)
	}
	digest := sha256.New()
	writer := io.MultiWriter(file, digest)
	remaining := size
	for remaining > 0 {
		next := min(remaining, int64(len(pattern)))
		written, writeErr := writer.Write(pattern[:int(next)])
		if writeErr != nil || written != int(next) {
			_ = file.Close()
			t.Fatalf("write Pion relay-cut fixture: bytes=%d err=%v", written, writeErr)
		}
		remaining -= next
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", digest.Sum(nil)); got != wantSHA256 {
		t.Fatalf("Pion relay-cut fixture SHA-256 = %s, want %s", got, wantSHA256)
	}
}

func assertV2FileSHA256(t *testing.T, filename string, wantBytes int64, wantSHA256 string) string {
	t.Helper()
	information, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if information.Size() != wantBytes {
		t.Fatalf("%s size = %d, want %d", filename, information.Size(), wantBytes)
	}
	got := v2FileSHA256(t, filename)
	if got != wantSHA256 {
		t.Fatalf("%s SHA-256 = %s, want %s", filename, got, wantSHA256)
	}
	return got
}

func v2FileSHA256(t *testing.T, filename string) string {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("hash %s: %v", filename, errors.Join(copyErr, closeErr))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func assertV2File(t *testing.T, filename string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("%s = %q, want %q", filename, actual, expected)
	}
}

func TestLongV2ProcessResumesDurableOutputAfterReceiverCrash(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2DurableResumeScenario)
	binaries := loadE2EBinaries(t)
	root := filepath.Join(t.TempDir(), "resume-tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, v2ResumeFileBytes)
	for index := range v2ResumeFileCount {
		name := filepath.Join(root, fmt.Sprintf("file-%03d.bin", index))
		if err := os.WriteFile(name, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	relay := startV2Process(
		t, scenario, v2RelayComponent, binaries.relay,
		"-listen", "127.0.0.1:0", "-state-dir", filepath.Join(t.TempDir(), "relay-state"),
	)
	address := waitV2RelayReady(t, relay)
	share := startV2Process(
		t, scenario, v2WindShareShareComponent, binaries.windshare, "share", root, "--relay", "ws://"+address,
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	capabilitySecrets := v2CapabilityForbiddenValues(shareLink)
	output := testoutputroot.New(t).RootPath
	firstOutput := filepath.Join(output, "resume-tree", "file-000.bin")

	interrupted := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
	waitV2PublishedFile(t, interrupted, firstOutput)
	interrupted.stop(t)

	resumed := startTracedV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
	resumed.forbidStderr(capabilitySecrets...)
	resumed.forbidUserTrace(append(capabilitySecrets, root, filepath.Base(root), output)...)
	// Recovery performs native witness revalidation before transferring the
	// interrupted tail, so it retains a scenario ceiling distinct from readiness.
	if err := resumed.waitWithin(t, v2DurableResumeProcessTimeout); err != nil {
		t.Fatalf(
			"resumed receiver failed: %v; receiver stdout=%q stderr=%q; sender stdout=%q stderr=%q; relay stdout=%q stderr=%q",
			err,
			resumed.stdout.diagnosticString(), resumed.stderr.diagnosticString(),
			share.stdout.diagnosticString(), share.stderr.diagnosticString(),
			relay.stdout.diagnosticString(), relay.stderr.diagnosticString(),
		)
	}
	requireV2UserTraceFact(t, resumed, "transfer_settled", "result_status", "success")
	// A fresh output session would hit the already-published first path and the
	// no-replace contract would make this command fail. Successful completion is
	// therefore the durable-resume oracle without exposing backend reopen state.
	assertV2File(t, firstOutput, payload)
	assertV2File(t, filepath.Join(output, "resume-tree", fmt.Sprintf("file-%03d.bin", v2ResumeFileCount-1)), payload)
	scenario.requireSuccess(t)
}

func waitV2PublishedFile(t *testing.T, process *v2Process, filename string) {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ArtifactCheckpointMilestone, phaseContext)
	deadline := time.Now().Add(v2ProcessTimeout)
	for time.Now().Before(deadline) {
		if information, err := os.Stat(filename); err == nil && information.Size() == v2ResumeFileBytes {
			select {
			case <-process.done:
				t.Fatalf("receiver completed before the crash checkpoint; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
			default:
				process.scenario.succeedPhase(t, phase, phaseContext)
				return
			}
		}
		select {
		case <-process.done:
			t.Fatalf("receiver exited before publishing the crash checkpoint: %v; stdout=%q stderr=%q", process.err, process.stdout.String(), process.stderr.String())
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("receiver did not publish the crash checkpoint; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
}

func assertV2OutputInventory(t *testing.T, root string, expected map[string]bool) {
	t.Helper()
	remaining := make(map[string]bool, len(expected))
	maps.Copy(remaining, expected)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == v2OutputControlDirectory {
			if !entry.IsDir() {
				return fmt.Errorf("output control namespace is not a directory")
			}
			// Recovery metadata has its own durable-state tests. This oracle owns the
			// complete user-visible namespace and admits only its reserved sibling.
			return filepath.SkipDir
		}
		wantDirectory, ok := remaining[relative]
		if !ok {
			return fmt.Errorf("unexpected output artifact %q", relative)
		}
		if entry.IsDir() != wantDirectory {
			return fmt.Errorf("output artifact %q directory=%t want=%t", relative, entry.IsDir(), wantDirectory)
		}
		delete(remaining, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("output inventory is missing artifacts: %#v", remaining)
	}
}
