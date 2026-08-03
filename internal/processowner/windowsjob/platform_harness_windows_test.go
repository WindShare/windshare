//go:build windows

package windowsjob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

type windowsSupervisorResult struct {
	status ownerprotocol.Settlement
	stdout string
	stderr string
	err    error
}

func runSupervisorPlatformAccepted(
	request supervisionRequest,
	settlements *settlementSink,
	control *os.File,
	rawInput *os.File,
	ready io.Writer,
) error {
	return runSupervisorPlatformWithDecision(
		request,
		settlements,
		control,
		rawInput,
		ready,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		0,
	)
}

func runSupervisorPlatformWithDecision(
	request supervisionRequest,
	settlements *settlementSink,
	control *os.File,
	rawInput *os.File,
	ready io.Writer,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
) error {
	evidenceReader, evidenceWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create test start-evidence pipe: %w", err)
	}
	decisionReader, decisionWriter, err := os.Pipe()
	if err != nil {
		return errors.Join(
			fmt.Errorf("create test start-decision pipe: %w", err),
			evidenceReader.Close(),
			evidenceWriter.Close(),
		)
	}
	consumerResult := make(chan error, 1)
	go func() {
		defer evidenceReader.Close()
		defer decisionWriter.Close()
		evidence, readErr := ownerprotocol.ReadFrame[ownerprotocol.StartEvidence](evidenceReader)
		if errors.Is(readErr, io.EOF) {
			consumerResult <- nil
			return
		}
		if readErr != nil {
			consumerResult <- readErr
			return
		}
		trailing, trailingErr := io.ReadAll(evidenceReader)
		if trailingErr != nil || len(trailing) != 0 {
			consumerResult <- errors.Join(
				trailingErr,
				errors.New("test start-evidence stream contains trailing bytes"),
			)
			return
		}
		if validationErr := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request.Protocol); validationErr != nil {
			consumerResult <- validationErr
			return
		}
		if decisionDelay > 0 {
			time.Sleep(decisionDelay)
		}
		consumerResult <- ownerprotocol.WriteFrame(
			decisionWriter,
			ownerprotocol.NewStartDecision(evidence, outcome, failureCode, failureMessage),
		)
	}()
	runErr := runSupervisorPlatform(
		request,
		settlements,
		control,
		rawInput,
		newStartGate(evidenceWriter, decisionReader, request.Protocol),
		ready,
	)
	ownerCloseErr := errors.Join(closeOptionalFile(evidenceWriter), closeOptionalFile(decisionReader))
	return errors.Join(runErr, ownerCloseErr, <-consumerResult)
}

func runWindowsSupervisor(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	return runWindowsSupervisorWithStartDecision(
		t,
		request,
		rawInput,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
	)
}

func runWindowsSupervisorWithStartDecision(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
) windowsSupervisorResult {
	return runWindowsSupervisorWithDelayedStartDecision(
		t,
		request,
		rawInput,
		outcome,
		failureCode,
		failureMessage,
		0,
	)
}

func runWindowsSupervisorWithDelayedStartDecision(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		rawInput,
		outcome,
		failureCode,
		failureMessage,
		decisionDelay,
		false,
		false,
	)
}

func runWindowsSupervisorWithPreloadedStop(
	t *testing.T,
	request supervisionRequest,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		nil,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		decisionDelay,
		true,
		false,
	)
}

func runWindowsSupervisorWithPreloadedParentLoss(
	t *testing.T,
	request supervisionRequest,
	decisionDelay time.Duration,
) windowsSupervisorResult {
	return runWindowsSupervisorWithStartBoundary(
		t,
		request,
		nil,
		ownerprotocol.StartDecisionAccepted,
		"",
		"",
		decisionDelay,
		false,
		true,
	)
}

func runWindowsSupervisorWithStartBoundary(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
	outcome string,
	failureCode string,
	failureMessage string,
	decisionDelay time.Duration,
	preloadStop bool,
	preloadParentLoss bool,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	defer parentWriter.Close()
	request.ParentHandle = parentReader.Fd()
	if preloadParentLoss {
		if err := parentWriter.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	settlements, output := newTestSettlementSink(t, request)
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	if preloadStop {
		control := ownerprotocol.Control{
			SchemaVersion: ownerprotocol.ControlSchemaVersion,
			Identity:      request.Identity,
			Reason:        ownerprotocol.ControlReasonStop,
		}
		if err := errors.Join(ownerprotocol.WriteFrame(controlWriter, control), controlWriter.Close()); err != nil {
			t.Fatal(err)
		}
	}
	rawReader, rawWrite := exactRawInputReader(t, request, rawInput)
	runErr := runSupervisorPlatformWithDecision(
		request,
		settlements,
		controlReader,
		rawReader,
		io.Discard,
		outcome,
		failureCode,
		failureMessage,
		decisionDelay,
	)
	if closeErr := closeOptionalFile(rawReader); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if rawWrite != nil {
		if writeErr := <-rawWrite; runErr == nil && writeErr != nil {
			runErr = writeErr
		}
	}
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

type acceptedStartGatePipes struct {
	evidenceReader *os.File
	evidenceWriter *os.File
	decisionReader *os.File
	decisionWriter *os.File
}

func newAcceptedStartGatePipes(t *testing.T) *acceptedStartGatePipes {
	t.Helper()
	evidenceReader, evidenceWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	decisionReader, decisionWriter, err := os.Pipe()
	if err != nil {
		_ = evidenceReader.Close()
		_ = evidenceWriter.Close()
		t.Fatal(err)
	}
	return &acceptedStartGatePipes{
		evidenceReader: evidenceReader,
		evidenceWriter: evidenceWriter,
		decisionReader: decisionReader,
		decisionWriter: decisionWriter,
	}
}

func (pipes *acceptedStartGatePipes) childFiles() []*os.File {
	return []*os.File{pipes.evidenceWriter, pipes.decisionReader}
}

func (pipes *acceptedStartGatePipes) arguments() []string {
	return []string{
		"--start-evidence-handle", strconv.FormatUint(uint64(pipes.evidenceWriter.Fd()), 10),
		"--start-decision-handle", strconv.FormatUint(uint64(pipes.decisionReader.Fd()), 10),
	}
}

func (pipes *acceptedStartGatePipes) consume(request ownerprotocol.Request) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer pipes.evidenceReader.Close()
		defer pipes.decisionWriter.Close()
		evidence, err := ownerprotocol.ReadFrame[ownerprotocol.StartEvidence](pipes.evidenceReader)
		if errors.Is(err, io.EOF) {
			result <- nil
			return
		}
		if err != nil {
			result <- err
			return
		}
		trailing, trailingErr := io.ReadAll(pipes.evidenceReader)
		if trailingErr != nil || len(trailing) != 0 {
			result <- errors.Join(trailingErr, errors.New("test start-evidence stream contains trailing bytes"))
			return
		}
		if err := ownerprotocol.ValidateStartEvidenceForRequest(evidence, request); err != nil {
			result <- err
			return
		}
		result <- ownerprotocol.WriteFrame(
			pipes.decisionWriter,
			ownerprotocol.NewStartDecision(evidence, ownerprotocol.StartDecisionAccepted, "", ""),
		)
	}()
	return result
}

func runWindowsSuperviseCommandInProcess(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	startPipes := newAcceptedStartGatePipes(t)
	startResult := startPipes.consume(request.Protocol)
	var rawReader, rawWriter *os.File
	if request.Stdin != nil {
		rawReader, rawWriter, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []*os.File{
		statusReader, statusWriter, controlReader, controlWriter, parentReader, parentWriter,
		rawReader, rawWriter,
		startPipes.evidenceReader, startPipes.evidenceWriter,
		startPipes.decisionReader, startPipes.decisionWriter,
	} {
		if file != nil {
			defer file.Close()
		}
	}
	statusHandle := duplicateSuperviseEndpoint(t, statusWriter)
	controlHandle := duplicateSuperviseEndpoint(t, controlReader)
	parentHandle := duplicateSuperviseEndpoint(t, parentReader)
	startEvidenceHandle := duplicateSuperviseEndpoint(t, startPipes.evidenceWriter)
	startDecisionHandle := duplicateSuperviseEndpoint(t, startPipes.decisionReader)
	if err := errors.Join(startPipes.evidenceWriter.Close(), startPipes.decisionReader.Close()); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		commandSupervise,
		"--status-handle", strconv.FormatUint(uint64(statusHandle), 10),
		"--control-handle", strconv.FormatUint(uint64(controlHandle), 10),
		"--parent-handle", strconv.FormatUint(uint64(parentHandle), 10),
		"--start-evidence-handle", strconv.FormatUint(uint64(startEvidenceHandle), 10),
		"--start-decision-handle", strconv.FormatUint(uint64(startDecisionHandle), 10),
		"--ready-stdout",
	}
	var rawWriteResult chan error
	if rawWriter != nil {
		rawHandle := duplicateSuperviseEndpoint(t, rawReader)
		arguments = append(arguments, "--input-handle", strconv.FormatUint(uint64(rawHandle), 10))
		rawWriteResult = make(chan error, 1)
		payload := bytes.Clone(rawInput)
		go func() {
			writeErr := writeAll(rawWriter, payload)
			for index := range payload {
				payload[index] = 0
			}
			rawWriteResult <- errors.Join(writeErr, rawWriter.Close())
		}()
	}
	var encodedRequest bytes.Buffer
	if err := ownerprotocol.WriteFrame(&encodedRequest, request.Protocol); err != nil {
		t.Fatal(err)
	}
	var ready bytes.Buffer
	runErr := runCommandWithReady(arguments, &encodedRequest, &ready)
	runErr = errors.Join(runErr, <-startResult)
	if ready.String() != string([]byte{ownerReadyByte}) {
		return windowsSupervisorResult{err: errors.Join(runErr, fmt.Errorf("readiness = %v", ready.Bytes()))}
	}
	if rawWriteResult != nil {
		runErr = errors.Join(runErr, <-rawWriteResult)
	}
	_ = statusWriter.Close()
	_ = controlWriter.Close()
	_ = parentWriter.Close()
	result := windowsSupervisorResult{err: runErr}
	settlement, statusErr := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](statusReader)
	if statusErr == nil {
		statusErr = ownerprotocol.ValidateSettlementForRequest(settlement, request.Protocol)
	}
	result.err = errors.Join(result.err, statusErr)
	if statusErr == nil {
		result.status = settlement
	}
	return result
}

func duplicateSuperviseEndpoint(t *testing.T, file *os.File) windows.Handle {
	t.Helper()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(file.Fd()), windows.CurrentProcess(), &duplicate,
		0, true, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		t.Fatal(err)
	}
	return duplicate
}

func runWindowsSupervisorProcess(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	return runWindowsSupervisorProcessAt(t, request, rawInput)
}

func runWindowsSupervisorProcessAt(t *testing.T, request supervisionRequest, rawInput []byte) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
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
		_ = statusReader.Close()
		_ = statusWriter.Close()
		t.Fatal(err)
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		t.Fatal(err)
	}
	rawReader, rawWrite := exactRawInputReader(t, request, rawInput)
	startPipes := newAcceptedStartGatePipes(t)
	startResult := startPipes.consume(request.Protocol)
	childEndpoints := []*os.File{statusWriter, controlReader, parentReader}
	childEndpoints = append(childEndpoints, startPipes.childFiles()...)
	if rawReader != nil {
		childEndpoints = append(childEndpoints, rawReader)
	}
	for _, endpoint := range childEndpoints {
		if err := windows.SetHandleInformation(
			windows.Handle(endpoint.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
		); err != nil {
			t.Fatal(err)
		}
	}
	var requestBytes bytes.Buffer
	if err := ownerprotocol.WriteFrame(&requestBytes, request.Protocol); err != nil {
		t.Fatal(err)
	}
	defer parentWriter.Close()
	defer controlWriter.Close()
	defer statusReader.Close()
	arguments := []string{
		commandSupervise,
		"--status-handle", strconv.FormatUint(uint64(statusWriter.Fd()), 10),
		"--control-handle", strconv.FormatUint(uint64(controlReader.Fd()), 10),
		"--parent-handle", strconv.FormatUint(uint64(parentReader.Fd()), 10),
		"--ready-stdout",
	}
	arguments = append(arguments, startPipes.arguments()...)
	if rawReader != nil {
		arguments = append(arguments, "--input-handle", strconv.FormatUint(uint64(rawReader.Fd()), 10))
	}
	command := exec.Command(
		executable,
		arguments...,
	)
	replacements := map[string]string{testHelperEnvironment: "1"}
	if coverageDirectory != "" {
		replacements["GOCOVERDIR"] = coverageDirectory
	}
	command.Env = replaceEnvironment(os.Environ(), replacements)
	inheritedHandles := make([]syscall.Handle, 0, len(childEndpoints))
	for _, endpoint := range childEndpoints {
		inheritedHandles = append(inheritedHandles, syscall.Handle(endpoint.Fd()))
	}
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, AdditionalInheritedHandles: inheritedHandles,
	}
	command.Stdin = bytes.NewReader(requestBytes.Bytes())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		for _, endpoint := range childEndpoints {
			_ = endpoint.Close()
		}
		t.Fatal(err)
	}
	for _, endpoint := range childEndpoints {
		if err := endpoint.Close(); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
	case <-time.After(testMarkerWaitLimit):
		_ = command.Process.Kill()
		waitErr = <-waitResult
		t.Fatalf("owner process exceeded its bounded test lease: %v", waitErr)
	}
	if rawWrite != nil {
		if writeErr := <-rawWrite; waitErr == nil && writeErr != nil {
			waitErr = writeErr
		}
	}
	waitErr = errors.Join(waitErr, <-startResult)
	_ = controlWriter.Close()
	_ = parentWriter.Close()
	output := stdout.Bytes()
	if len(output) == 0 || output[0] != ownerReadyByte {
		t.Fatalf("owner readiness output = %v", output)
	}
	result := windowsSupervisorResult{stdout: string(output[1:]), stderr: stderr.String(), err: waitErr}
	settlement, statusErr := ownerprotocol.ReadLineDocument[ownerprotocol.Settlement](statusReader)
	if statusErr == nil {
		statusErr = ownerprotocol.ValidateSettlementForRequest(settlement, request.Protocol)
	}
	if result.err == nil && statusErr != nil {
		result.err = statusErr
	}
	if statusErr == nil {
		result.status = settlement
	}
	return result
}

func exactRawInputReader(
	t *testing.T,
	request supervisionRequest,
	rawInput []byte,
) (*os.File, <-chan error) {
	t.Helper()
	if request.Stdin == nil {
		if len(rawInput) != 0 {
			t.Fatal("test supplied undeclared raw stdin")
		}
		return nil, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
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

func runWindowsSupervisorThroughExpiringParent(
	t *testing.T,
	request supervisionRequest,
) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	ready := environmentValue(request.Environment, testMarkerEnvironment)
	if ready == "" {
		t.Fatal("parent-loss request has no readiness marker")
	}
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	request.ParentHandle = parentReader.Fd()
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
	settlements, output := newTestSettlementSink(t, request)
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	finished := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		finished <- runSupervisorPlatformAccepted(request, settlements, controlReader, nil, io.Discard)
		close(done)
	}()
	if err := waitForMarker(ready, done); err != nil {
		return windowsSupervisorResult{err: err}
	}
	if err := parentWriter.Close(); err != nil {
		return windowsSupervisorResult{err: err}
	}
	runErr := <-finished
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

func runWindowsSupervisorWithControl(t *testing.T, request supervisionRequest) windowsSupervisorResult {
	t.Helper()
	request, coverageDirectory := prepareChildProcessEnvironment(t, request)
	parentReader, parentWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer parentReader.Close()
	defer parentWriter.Close()
	request.ParentHandle = parentReader.Fd()
	t.Setenv(testHelperEnvironment, "1")
	if coverageDirectory != "" {
		t.Setenv("GOCOVERDIR", coverageDirectory)
	}
	settlements, output := newTestSettlementSink(t, request)
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlReader.Close()
	defer controlWriter.Close()
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
		control := ownerprotocol.Control{
			SchemaVersion: ownerprotocol.ControlSchemaVersion,
			Identity:      request.Identity,
			Reason:        ownerprotocol.ControlReasonStop,
		}
		controlResult <- errors.Join(
			ownerprotocol.WriteFrame(controlWriter, control),
			controlWriter.Close(),
		)
	}()
	rawReader, _ := exactRawInputReader(t, request, nil)
	runErr := runSupervisorPlatformAccepted(request, settlements, controlReader, rawReader, io.Discard)
	_ = closeOptionalFile(rawReader)
	if controlErr := <-controlResult; runErr == nil && controlErr != nil {
		runErr = controlErr
	}
	result := windowsSupervisorResult{err: runErr}
	if runErr == nil {
		result.status = decodeTestSettlement(t, output)
	}
	return result
}

func prepareChildProcessEnvironment(t *testing.T, request supervisionRequest) (supervisionRequest, string) {
	t.Helper()
	coverageDirectory := ""
	if testing.CoverMode() != "" {
		coverageDirectory = t.TempDir()
		request.Environment = upsertEnvironmentEntry(request.Environment, "GOCOVERDIR", coverageDirectory)
	}
	request.Protocol = ownerprotocol.NewRequest(request.Identity, ownerprotocol.Command{
		Executable: request.Executable, Arguments: request.Arguments,
		WorkingDirectory: request.WorkingDirectory, Environment: request.Environment, Stdin: request.Stdin,
	}, request.DeadlineMilliseconds, request.TerminationGraceMilliseconds)
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

func windowsIntegrationRequest(t *testing.T, target string, deadlineMS int64) supervisionRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Clean(t.TempDir())
	identity := ownerprotocol.Identity{
		RunID: "windows-owner-tests", OperationID: "windows-integration-" + target, Scenario: target,
	}
	protocolRequest := ownerprotocol.NewRequest(identity, ownerprotocol.Command{
		Executable: filepath.Clean(executable), Arguments: []string{}, WorkingDirectory: cwd,
		Environment: targetEnvironment(target, "", cwd),
	}, deadlineMS, 3_000)
	return newSupervisionRequest(protocolRequest, 0)
}

func targetEnvironment(target, marker, cwd string) []ownerprotocol.EnvironmentEntry {
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
	environment := make([]ownerprotocol.EnvironmentEntry, 0, len(values))
	for name, value := range values {
		environment = append(environment, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(environment, func(left, right int) bool {
		return strings.ToLower(environment[left].Name) < strings.ToLower(environment[right].Name)
	})
	return environment
}

func upsertEnvironmentEntry(environment []ownerprotocol.EnvironmentEntry, name, value string) []ownerprotocol.EnvironmentEntry {
	result := make([]ownerprotocol.EnvironmentEntry, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			if !replaced {
				result = append(result, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, ownerprotocol.EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool {
		return strings.ToLower(result[left].Name) < strings.ToLower(result[right].Name)
	})
	return result
}

func environmentValue(environment []ownerprotocol.EnvironmentEntry, name string) string {
	for _, entry := range environment {
		if strings.EqualFold(entry.Name, name) {
			return entry.Value
		}
	}
	return ""
}

func settledInputOutcome(request supervisionRequest) string {
	if request.Stdin == nil {
		return ownerprotocol.InputNotRequested
	}
	return ownerprotocol.InputDelivered
}

func assertTreeStatus(t *testing.T, status ownerprotocol.Settlement, request supervisionRequest, reason string, timedOut bool, exitCode uint32) {
	t.Helper()
	if status.SchemaVersion != ownerprotocol.SettlementSchemaVersion || status.Identity != request.Identity {
		t.Fatalf("status identity = %#v", status)
	}
	if status.TreeState != ownerprotocol.TreeProvenEmpty || status.TerminationReason != reason ||
		(reason == ownerprotocol.TerminationDeadline) != timedOut {
		t.Fatalf("status outcome = %#v", status)
	}
	if status.Platform.ActiveProcessCount == nil || *status.Platform.ActiveProcessCount != 0 ||
		status.Platform.Root == nil || status.Platform.Root.ExitCode == nil ||
		uint32(*status.Platform.Root.ExitCode) != exitCode {
		t.Fatalf("status authority = %#v", status)
	}
	if status.Input.Outcome != settledInputOutcome(request) {
		t.Fatalf("status input outcome = %q, want %q", status.Input.Outcome, settledInputOutcome(request))
	}
}

func assertPrivateTreeStatus(
	t *testing.T,
	status ownerprotocol.Settlement,
	request supervisionRequest,
	reason string,
	timedOut bool,
) {
	t.Helper()
	if status.Platform.Root == nil || status.Platform.Root.ExitCode == nil {
		t.Fatalf("status omits exact root termination evidence: %#v", status)
	}
	exitCode := uint32(*status.Platform.Root.ExitCode)
	assertTreeStatus(t, status, request, reason, timedOut, exitCode)
	if exitCode == 0 || exitCode == windowsStillActiveExitCode || exitCode&uint32(windows.APPLICATION_ERROR) == 0 {
		t.Fatalf("status root exit code is not in the private termination class: %#x", exitCode)
	}
}
