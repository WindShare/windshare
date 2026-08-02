package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
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
	v2ResumeFileCount              = 2
	v2ResumeFileBytes              = 8 << 20
	v2FailureLogStreamBytes        = 4 << 10
	v2UsageExitCode          int64 = 2
	v2InvalidLinkDiagnostic        = "get: invalid capability link\n"
	v2OutputControlDirectory       = ".windshare-output"

	v2PionRelayCutPayloadBytes  int64 = 32 << 20
	v2PionRelayCutBlockBytes          = 64 << 10
	v2PionRelayCutPayloadSHA256       = "e09320c5b00b34bb704802136c599a95b3996332ba84d7c7f21112b6231b6bd0"

	v2RelayComponent          = "wsrelay"
	v2WindShareShareComponent = "windshare_share"
	v2WindShareGetComponent   = "windshare_get"
)

const (
	v2ProgressiveCatalogScenario = "v2-progressive-catalog"
	v2PionRelayCutScenario       = "v2-pion-relay-cut"
	v2DurableResumeScenario      = "v2-durable-resume"
	v2InvalidLinkScenario        = "v2-invalid-link-diagnostics"
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
	scenario   *v2Scenario
	component  string
	owner      *testprocess.Owner
	owned      *testprocess.Process
	stdout     *processOutputView
	stderr     *processOutputView
	done       chan struct{}
	settlement ownerprotocol.Settlement
	err        error
	closeOnce  sync.Once
	closeDone  chan struct{}
	closeErr   error
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
		Identity: ownerprotocol.Identity{
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
		process.settlement, process.err = process.owned.Wait(context.Background())
		close(process.done)
	}()
	scenario.succeedPhase(t, phase, phaseContext)
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
		settlement, err := process.owned.Stop(closeContext)
		stopErr = err
		if settlement.SchemaVersion != "" {
			cleanupErr = testprocess.RequireTreeEmpty(settlement)
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

func (process *v2Process) wait(t *testing.T) error {
	return process.waitWithin(t, v2ProcessTimeout)
}

func (process *v2Process) waitWithin(t *testing.T, timeout time.Duration) error {
	t.Helper()
	settlement, err := process.waitSettlementWithin(t, timeout)
	return testprocess.RequireSuccess(settlement, err)
}

func (process *v2Process) waitSettlementWithin(
	t *testing.T,
	timeout time.Duration,
) (ownerprotocol.Settlement, error) {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessWaitMilestone, phaseContext)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
		if process.err != nil {
			return process.settlement, errors.Join(process.err, phase.Fail(v2ProcessWaitFailureReason))
		}
		return process.settlement, phase.Succeed(phaseContext)
	case <-timer.C:
		cleanupErr := process.close()
		t.Fatalf(
			"process timeout: run_id=%s operation_id=%s scenario=%s component=%s cleanup=%v stdout=%q stderr=%q",
			process.scenario.operation.RunID(), process.scenario.operation.ID(), process.scenario.operation.Scenario(), process.component,
			cleanupErr, process.stdout.diagnosticString(), process.stderr.diagnosticString(),
		)
		return ownerprotocol.Settlement{}, context.DeadlineExceeded
	}
}

func waitV2Match(t *testing.T, process *v2Process, expression *regexp.Regexp, stream *processOutputView) string {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessReadinessMilestone, phaseContext)
	deadline := time.Now().Add(v2ProcessTimeout)
	for time.Now().Before(deadline) {
		if match := expression.FindStringSubmatch(stream.String()); match != nil {
			process.scenario.succeedPhase(t, phase, phaseContext)
			return match[1]
		}
		select {
		case <-process.done:
			t.Fatalf("process exited before readiness: %v; stdout=%q stderr=%q", process.err, process.stdout.String(), process.stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("readiness timeout; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
	return ""
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
	payload, err := ownerprotocol.DecodeCanonical[testrun.ListenerReadyContext](event.Payload)
	if err != nil {
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

func TestV2ProcessProgressiveCatalogConcurrentReceiversAndSelection(t *testing.T) {
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
	selected := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare,
		"get", shareLink, "-o", selectedOutput, "--only", "tree/nested/a.txt",
	)
	if err := selected.wait(t); err != nil {
		t.Fatalf("selected receiver failed: %v; stdout=%q stderr=%q", err, selected.stdout.String(), selected.stderr.String())
	}
	assertV2File(t, filepath.Join(selectedOutput, "tree", "nested", "a.txt"), []byte("selected-content"))
	assertV2OutputInventory(t, selectedOutput, map[string]bool{
		"tree":              true,
		"tree/nested":       true,
		"tree/nested/a.txt": false,
	})
	scenario.requireSuccess(t)
}

func TestV2ProcessTransfersExactPayloadOverPionAfterRelayCut(t *testing.T) {
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
	output := testoutputroot.New(t).RootPath
	receiver := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
	// Stderr remains diagnostic output; the owner-routed event is the typed,
	// operation-bound authority that the receiver actually adopted a direct lane.
	waitV2ProcessTrace(t, receiver, v2ReceiverDirectLaneMilestone, testrun.OutcomeSucceeded)
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
	// The counter includes every downstream TCP byte, including HTTP and relay
	// framing. Suite-02 does not compress content, so a value below plaintext
	// size proves the relay could not have delivered the complete fixture.
	if relayDownstream == 0 || relayDownstream >= uint64(v2PionRelayCutPayloadBytes) {
		t.Fatalf(
			"relay downstream at cut = %d bytes, want a positive value below payload size %d",
			relayDownstream,
			v2PionRelayCutPayloadBytes,
		)
	}
	if err := receiver.wait(t); err != nil {
		t.Fatalf(
			"receiver failed after authenticated Pion activation and relay cut: %v; stdout=%q stderr=%q",
			err,
			receiver.stdout.String(),
			receiver.stderr.String(),
		)
	}

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

func TestV2ProcessResumesDurableOutputAfterReceiverCrash(t *testing.T) {
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
	output := testoutputroot.New(t).RootPath
	firstOutput := filepath.Join(output, "resume-tree", "file-000.bin")

	interrupted := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
	waitV2PublishedFile(t, interrupted, firstOutput)
	interrupted.stop(t)

	resumed := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", shareLink, "-o", output,
	)
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

func TestV2ProcessErrorIncludesDiagnostics(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2InvalidLinkScenario)
	binaries := loadE2EBinaries(t)
	process := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare, "get", "not-a-link",
	)
	settlement, err := process.waitSettlementWithin(t, v2ProcessTimeout)
	if err != nil {
		t.Fatalf("invalid-link process ownership failed: %v", err)
	}
	if err := testprocess.RequireTreeEmpty(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.Target.Outcome != ownerprotocol.TargetExited ||
		settlement.Target.ExitCode == nil || *settlement.Target.ExitCode != v2UsageExitCode ||
		process.stdout.String() != "" || process.stderr.String() != v2InvalidLinkDiagnostic {
		t.Fatalf(
			"invalid link result: settlement=%#v stdout=%q stderr=%q",
			settlement, process.stdout.String(), process.stderr.String(),
		)
	}
	scenario.requireSuccess(t)
}

func assertV2OutputInventory(t *testing.T, root string, expected map[string]bool) {
	t.Helper()
	remaining := make(map[string]bool, len(expected))
	for path, directory := range expected {
		remaining[path] = directory
	}
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
