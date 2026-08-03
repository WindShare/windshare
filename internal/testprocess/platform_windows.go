//go:build windows

package testprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

const (
	windowsOwnerReadyByte       byte = 0xa5
	windowsOwnerStartupWait          = 10 * time.Second
	windowsOwnerTransportMargin      = 5 * time.Second
	windowsOwnerForcedJoinWait       = 2 * time.Second
)

type windowsClientPipes struct {
	statusReader        *os.File
	statusWriter        *os.File
	controlReader       *os.File
	controlWriter       *os.File
	eventReader         *os.File
	eventWriter         *os.File
	parentReader        *os.File
	parentWriter        *os.File
	inputReader         *os.File
	inputWriter         *os.File
	startEvidenceReader *os.File
	startEvidenceWriter *os.File
	startDecisionReader *os.File
	startDecisionWriter *os.File
}

func startPlatform(
	ctx context.Context,
	helperPath string,
	spec Spec,
	request protocol.Request,
	output *processOutput,
) (platformSession, error) {
	var encodedRequest bytes.Buffer
	if err := protocol.WriteFrame(&encodedRequest, request); err != nil {
		return nil, fmt.Errorf("encode process-owner request: %w", err)
	}
	pipes, err := openWindowsClientPipes(request.Command.Stdin != nil)
	if err != nil {
		return nil, err
	}
	closeAll := func() error { return pipes.closeAll() }
	inherited := []struct {
		file  *os.File
		label string
	}{
		{pipes.statusWriter, "settlement"},
		{pipes.controlReader, "control"},
		{pipes.eventWriter, "test event"},
		{pipes.parentReader, "parent liveness"},
		{pipes.startEvidenceWriter, "start evidence"},
		{pipes.startDecisionReader, "start decision"},
	}
	if pipes.inputReader != nil {
		inherited = append(inherited, struct {
			file  *os.File
			label string
		}{pipes.inputReader, "raw input"})
	}
	inheritedHandles := make([]syscall.Handle, 0, len(inherited))
	for _, endpoint := range inherited {
		if err := makeWindowsClientHandleInheritable(endpoint.file, endpoint.label); err != nil {
			return nil, errors.Join(err, closeAll())
		}
		inheritedHandles = append(inheritedHandles, syscall.Handle(endpoint.file.Fd()))
	}
	arguments := []string{
		"supervise",
		"--status-handle", decimalHandle(pipes.statusWriter),
		"--control-handle", decimalHandle(pipes.controlReader),
		"--event-handle", decimalHandle(pipes.eventWriter),
		"--parent-handle", decimalHandle(pipes.parentReader),
		"--start-evidence-handle", decimalHandle(pipes.startEvidenceWriter),
		"--start-decision-handle", decimalHandle(pipes.startDecisionReader),
		"--ready-stdout",
	}
	if pipes.inputReader != nil {
		arguments = append(arguments, "--input-handle", decimalHandle(pipes.inputReader))
	}
	command := exec.Command(helperPath, arguments...)
	command.Dir = spec.Command.WorkingDirectory
	command.Stdin = bytes.NewReader(encodedRequest.Bytes())
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: inheritedHandles,
	}
	ownerOutput, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner readiness channel: %w", err), closeAll())
	}
	ownerErrorOutput, err := command.StderrPipe()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create process-owner stderr channel: %w", err), ownerOutput.Close(), closeAll(),
		)
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start external process owner: %w", err),
			ownerOutput.Close(), ownerErrorOutput.Close(), closeAll(),
		)
	}
	childCloseErr := pipes.closeChildEnds()
	stderrResult := make(chan error, 1)
	go func() {
		stderrResult <- drainOwnedOutput(ownerErrorOutput, output.stderr, "owned stderr")
	}()
	inputResult := make(chan error, 1)
	if pipes.inputWriter == nil {
		inputResult <- nil
	} else {
		inputWriter := pipes.inputWriter
		go func() {
			_, writeErr := io.Copy(inputWriter, bytes.NewReader(spec.Command.Stdin))
			inputResult <- errors.Join(writeErr, inputWriter.Close())
		}()
	}
	statusResult := make(chan windowsStatusResult, 1)
	go readWindowsSettlement(pipes.statusReader, request, statusResult)
	startResult := make(chan startGateResult, 1)
	go func() {
		startResult <- completeStartGate(pipes.startEvidenceReader, pipes.startDecisionWriter, request)
	}()
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()

	readyResult := make(chan error, 1)
	go func() {
		ready := []byte{0}
		_, readErr := io.ReadFull(ownerOutput, ready)
		if readErr == nil && ready[0] != windowsOwnerReadyByte {
			readErr = fmt.Errorf("external process owner returned invalid readiness byte %#x", ready[0])
		}
		readyResult <- readErr
	}()
	startupTimer := time.NewTimer(windowsOwnerStartupWait)
	defer startupTimer.Stop()
	var readinessErr error
	readinessJoined := false
	select {
	case readinessErr = <-readyResult:
		readinessJoined = true
	case <-ctx.Done():
		readinessErr = fmt.Errorf("wait for external process-owner readiness: %w", ctx.Err())
	case <-startupTimer.C:
		readinessErr = errors.New("external process owner did not establish readiness within its startup lease")
	}
	if readinessErr != nil {
		killErr := command.Process.Kill()
		outputCloseErr := errors.Join(ownerOutput.Close(), ownerErrorOutput.Close())
		parentCloseErr := pipes.closeParentEnds()
		startupState := windowsStartupJoinState{readinessReady: readinessJoined}
		if readinessJoined {
			startupState.readinessErr = readinessErr
		}
		joined := startupState.collect(
			readyResult,
			statusResult,
			inputResult,
			stderrResult,
			waitResult,
			windowsOwnerForcedJoinWait,
		)
		if _, ok := errors.AsType[*exec.ExitError](startupState.waitErr); ok {
			killErr = nil
		}
		var joinErr error
		if !joined {
			joinErr = fmt.Errorf(
				"external process-owner startup tasks did not join: %s",
				strings.Join(startupState.pending(), ", "),
			)
		}
		return nil, errors.Join(
			readinessErr,
			killErr,
			startupState.inputErr,
			startupState.stderrErr,
			startupState.waitErr,
			joinErr,
			outputCloseErr,
			childCloseErr,
			parentCloseErr,
		)
	}
	stdoutResult := make(chan error, 1)
	go func() {
		stdoutResult <- drainOwnedOutput(ownerOutput, output.stdout, "owned stdout")
	}()
	return &windowsSession{
		command: command, stdout: stdoutResult, stderr: stderrResult, status: statusResult, waitResult: waitResult,
		inputResult: inputResult, startResult: startResult,
		controlWriter: pipes.controlWriter, eventReader: pipes.eventReader,
		parentWriter: pipes.parentWriter, inputWriter: pipes.inputWriter, statusReader: pipes.statusReader,
		stdoutReader: ownerOutput, stderrReader: ownerErrorOutput,
		startEvidence: pipes.startEvidenceReader, startDecision: pipes.startDecisionWriter,
		identity: request.Identity, initialErr: childCloseErr,
		lease: time.Duration(request.DeadlineMilliseconds+request.TerminationGraceMilliseconds)*time.Millisecond +
			windowsOwnerTransportMargin,
		retireGrace: time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond +
			windowsOwnerTransportMargin,
	}, nil
}

func openWindowsClientPipes(withInput bool) (*windowsClientPipes, error) {
	pipes := &windowsClientPipes{}
	var err error
	if pipes.statusReader, pipes.statusWriter, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("create settlement pipe: %w", err)
	}
	if pipes.controlReader, pipes.controlWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create control pipe: %w", err), pipes.closeAll())
	}
	if pipes.eventReader, pipes.eventWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create private test-event pipe: %w", err), pipes.closeAll())
	}
	if pipes.parentReader, pipes.parentWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create parent-liveness pipe: %w", err), pipes.closeAll())
	}
	if pipes.startEvidenceReader, pipes.startEvidenceWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create start-evidence pipe: %w", err), pipes.closeAll())
	}
	if pipes.startDecisionReader, pipes.startDecisionWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create start-decision pipe: %w", err), pipes.closeAll())
	}
	if withInput {
		if pipes.inputReader, pipes.inputWriter, err = os.Pipe(); err != nil {
			return nil, errors.Join(fmt.Errorf("create raw-input pipe: %w", err), pipes.closeAll())
		}
	}
	return pipes, nil
}

func (pipes *windowsClientPipes) closeChildEnds() error {
	return errors.Join(
		closeWindowsFile(&pipes.statusWriter),
		closeWindowsFile(&pipes.controlReader),
		closeWindowsFile(&pipes.eventWriter),
		closeWindowsFile(&pipes.parentReader),
		closeWindowsFile(&pipes.inputReader),
		closeWindowsFile(&pipes.startEvidenceWriter),
		closeWindowsFile(&pipes.startDecisionReader),
	)
}

func (pipes *windowsClientPipes) closeParentEnds() error {
	return errors.Join(
		closeWindowsFile(&pipes.statusReader),
		closeWindowsFile(&pipes.controlWriter),
		closeWindowsFile(&pipes.eventReader),
		closeWindowsFile(&pipes.parentWriter),
		closeWindowsFile(&pipes.inputWriter),
		closeWindowsFile(&pipes.startEvidenceReader),
		closeWindowsFile(&pipes.startDecisionWriter),
	)
}

func (pipes *windowsClientPipes) closeAll() error {
	return errors.Join(pipes.closeChildEnds(), pipes.closeParentEnds())
}

func closeWindowsFile(file **os.File) error {
	if file == nil || *file == nil {
		return nil
	}
	err := (*file).Close()
	*file = nil
	return err
}

func makeWindowsClientHandleInheritable(file *os.File, label string) error {
	if file == nil {
		return fmt.Errorf("%s handle is unavailable", label)
	}
	if err := windows.SetHandleInformation(
		windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT,
	); err != nil {
		return fmt.Errorf("make %s handle inheritable: %w", label, err)
	}
	return nil
}

func decimalHandle(file *os.File) string {
	return strconv.FormatUint(uint64(file.Fd()), 10)
}

type windowsStartupJoinState struct {
	readinessErr   error
	readinessReady bool
	status         windowsStatusResult
	statusReady    bool
	inputErr       error
	inputReady     bool
	stderrErr      error
	stderrReady    bool
	waitErr        error
	waitReady      bool
}

func (state *windowsStartupJoinState) complete() bool {
	return state.readinessReady && state.statusReady && state.inputReady && state.stderrReady && state.waitReady
}

func (state *windowsStartupJoinState) pending() []string {
	pending := make([]string, 0, 5)
	if !state.readinessReady {
		pending = append(pending, "readiness")
	}
	if !state.statusReady {
		pending = append(pending, "settlement")
	}
	if !state.inputReady {
		pending = append(pending, "input")
	}
	if !state.stderrReady {
		pending = append(pending, "stderr")
	}
	if !state.waitReady {
		pending = append(pending, "process wait")
	}
	return pending
}

func (state *windowsStartupJoinState) collect(
	readiness <-chan error,
	status <-chan windowsStatusResult,
	input <-chan error,
	stderr <-chan error,
	wait <-chan error,
	maximum time.Duration,
) bool {
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	for !state.complete() {
		var readinessChannel <-chan error
		if !state.readinessReady {
			readinessChannel = readiness
		}
		var statusChannel <-chan windowsStatusResult
		if !state.statusReady {
			statusChannel = status
		}
		var inputChannel, stderrChannel, waitChannel <-chan error
		if !state.inputReady {
			inputChannel = input
		}
		if !state.stderrReady {
			stderrChannel = stderr
		}
		if !state.waitReady {
			waitChannel = wait
		}
		select {
		case state.readinessErr = <-readinessChannel:
			state.readinessReady = true
		case state.status = <-statusChannel:
			state.statusReady = true
		case state.inputErr = <-inputChannel:
			state.inputReady = true
		case state.stderrErr = <-stderrChannel:
			state.stderrReady = true
		case state.waitErr = <-waitChannel:
			state.waitReady = true
		case <-deadline.C:
			return false
		}
	}
	return true
}
