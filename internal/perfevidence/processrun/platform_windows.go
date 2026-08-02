//go:build windows

package processrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
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

type windowsStatusResult struct {
	settlement protocol.Settlement
	err        error
}

type windowsPipes struct {
	statusReader        *os.File
	statusWriter        *os.File
	controlReader       *os.File
	controlWriter       *os.File
	eventReader         *os.File
	eventWriter         *os.File
	parentReader        *os.File
	parentWriter        *os.File
	startEvidenceReader *os.File
	startEvidenceWriter *os.File
	startDecisionReader *os.File
	startDecisionWriter *os.File
}

type windowsWaitState struct {
	status      windowsStatusResult
	statusReady bool
	start       startGateResult
	startReady  bool
	waitErr     error
	waitReady   bool
	stdoutErr   error
	stdoutReady bool
	stderrErr   error
	stderrReady bool
	eventErr    error
	eventReady  bool
}

func (state *windowsWaitState) complete() bool {
	return state.statusReady && state.startReady && state.waitReady &&
		state.stdoutReady && state.stderrReady && state.eventReady
}

type windowsSession struct {
	command       *exec.Cmd
	identity      protocol.Identity
	controlWriter *os.File
	parentWriter  *os.File
	statusReader  *os.File
	eventReader   *os.File
	stdoutReader  io.ReadCloser
	stderrReader  io.ReadCloser
	startEvidence *os.File
	startDecision *os.File
	status        <-chan windowsStatusResult
	startResult   <-chan startGateResult
	waitResult    <-chan error
	stdoutResult  <-chan error
	stderrResult  <-chan error
	eventResult   <-chan error
	initialErr    error
	lease         time.Duration
	retireGrace   time.Duration

	controlCloseOnce sync.Once
	controlCloseErr  error
	parentCloseOnce  sync.Once
	parentCloseErr   error
}

func startPlatform(
	ctx context.Context,
	helperPath string,
	spec Spec,
	request protocol.Request,
	output *processOutput,
) (platformSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start external process owner: %w", err)
	}
	var encodedRequest bytes.Buffer
	if err := protocol.WriteFrame(&encodedRequest, request); err != nil {
		return nil, fmt.Errorf("encode process-owner request: %w", err)
	}
	pipes, err := openWindowsPipes()
	if err != nil {
		return nil, err
	}
	inherited := []struct {
		file  *os.File
		label string
	}{
		{pipes.statusWriter, "settlement"},
		{pipes.controlReader, "control"},
		{pipes.eventWriter, "event"},
		{pipes.parentReader, "parent liveness"},
		{pipes.startEvidenceWriter, "start evidence"},
		{pipes.startDecisionReader, "start decision"},
	}
	inheritedHandles := make([]syscall.Handle, 0, len(inherited))
	for _, endpoint := range inherited {
		if err := makeWindowsHandleInheritable(endpoint.file, endpoint.label); err != nil {
			return nil, errors.Join(err, pipes.closeAll())
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
	command := exec.Command(helperPath, arguments...)
	command.Dir = spec.WorkingDirectory
	command.Env = ownerHelperEnvironment(os.Environ())
	command.Stdin = bytes.NewReader(encodedRequest.Bytes())
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: inheritedHandles,
	}
	ownerOutput, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner readiness channel: %w", err), pipes.closeAll())
	}
	ownerErrorOutput, err := command.StderrPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner stderr channel: %w", err), ownerOutput.Close(), pipes.closeAll())
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start external process owner: %w", err),
			ownerOutput.Close(), ownerErrorOutput.Close(), pipes.closeAll(),
		)
	}
	childCloseErr := pipes.closeChildEnds()

	statusResult := make(chan windowsStatusResult, 1)
	go func() {
		defer pipes.statusReader.Close()
		settlement, readErr := protocol.ReadLineDocument[protocol.Settlement](pipes.statusReader)
		if readErr == nil {
			readErr = protocol.ValidateSettlementForRequest(settlement, request)
		}
		statusResult <- windowsStatusResult{settlement: settlement, err: readErr}
	}()
	startResult := make(chan startGateResult, 1)
	go func() {
		startResult <- completeStartGate(
			pipes.startEvidenceReader,
			pipes.startDecisionWriter,
			request,
			spec.AuthorizeStart,
		)
	}()
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	stderrResult := make(chan error, 1)
	go func() { stderrResult <- drainOutput(ownerErrorOutput, output.stderr, "owned stderr") }()
	eventResult := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(io.Discard, pipes.eventReader)
		eventResult <- errors.Join(copyErr, pipes.eventReader.Close())
	}()
	readyResult := make(chan error, 1)
	stdoutResult := make(chan error, 1)
	go func() {
		ready := []byte{0}
		_, readErr := io.ReadFull(ownerOutput, ready)
		if readErr == nil && ready[0] != windowsOwnerReadyByte {
			readErr = fmt.Errorf("external process owner returned invalid readiness byte %#x", ready[0])
		}
		readyResult <- readErr
		if readErr != nil {
			stdoutResult <- readErr
			return
		}
		stdoutResult <- drainOutput(ownerOutput, output.stdout, "owned stdout")
	}()

	session := &windowsSession{
		command: command, identity: request.Identity,
		controlWriter: pipes.controlWriter, parentWriter: pipes.parentWriter,
		statusReader: pipes.statusReader, eventReader: pipes.eventReader,
		stdoutReader: ownerOutput, stderrReader: ownerErrorOutput,
		startEvidence: pipes.startEvidenceReader, startDecision: pipes.startDecisionWriter,
		status: statusResult, startResult: startResult, waitResult: waitResult,
		stdoutResult: stdoutResult, stderrResult: stderrResult, eventResult: eventResult,
		initialErr: childCloseErr,
		lease: time.Duration(request.DeadlineMilliseconds+request.TerminationGraceMilliseconds)*time.Millisecond +
			windowsOwnerTransportMargin,
		retireGrace: time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond +
			windowsOwnerTransportMargin,
	}
	startupTimer := time.NewTimer(windowsOwnerStartupWait)
	defer startupTimer.Stop()
	select {
	case readinessErr := <-readyResult:
		session.initialErr = errors.Join(session.initialErr, readinessErr)
	case <-ctx.Done():
		session.initialErr = errors.Join(session.initialErr, fmt.Errorf("wait for process-owner readiness: %w", context.Cause(ctx)))
		_ = session.closeControl()
		_ = session.closeParent()
		_ = session.closeStart()
	case <-startupTimer.C:
		session.initialErr = errors.Join(session.initialErr, errors.New("external process owner did not establish readiness within its startup lease"))
		_ = session.closeControl()
		_ = session.closeParent()
		_ = session.closeStart()
	}
	return session, nil
}

func openWindowsPipes() (*windowsPipes, error) {
	pipes := &windowsPipes{}
	var err error
	if pipes.statusReader, pipes.statusWriter, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("create settlement pipe: %w", err)
	}
	if pipes.controlReader, pipes.controlWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create control pipe: %w", err), pipes.closeAll())
	}
	if pipes.eventReader, pipes.eventWriter, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create event pipe: %w", err), pipes.closeAll())
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
	return pipes, nil
}

func (pipes *windowsPipes) closeChildEnds() error {
	return errors.Join(
		closeWindowsFile(&pipes.statusWriter),
		closeWindowsFile(&pipes.controlReader),
		closeWindowsFile(&pipes.eventWriter),
		closeWindowsFile(&pipes.parentReader),
		closeWindowsFile(&pipes.startEvidenceWriter),
		closeWindowsFile(&pipes.startDecisionReader),
	)
}

func (pipes *windowsPipes) closeParentEnds() error {
	return errors.Join(
		closeWindowsFile(&pipes.statusReader),
		closeWindowsFile(&pipes.controlWriter),
		closeWindowsFile(&pipes.eventReader),
		closeWindowsFile(&pipes.parentWriter),
		closeWindowsFile(&pipes.startEvidenceReader),
		closeWindowsFile(&pipes.startDecisionWriter),
	)
}

func (pipes *windowsPipes) closeAll() error {
	return errors.Join(pipes.closeChildEnds(), pipes.closeParentEnds())
}

func closeWindowsFile(file **os.File) error {
	if file == nil || *file == nil {
		return nil
	}
	err := (*file).Close()
	*file = nil
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func makeWindowsHandleInheritable(file *os.File, label string) error {
	if file == nil {
		return fmt.Errorf("%s handle is unavailable", label)
	}
	if err := windows.SetHandleInformation(
		windows.Handle(file.Fd()),
		windows.HANDLE_FLAG_INHERIT,
		windows.HANDLE_FLAG_INHERIT,
	); err != nil {
		return fmt.Errorf("make %s handle inheritable: %w", label, err)
	}
	return nil
}

func decimalHandle(file *os.File) string {
	return strconv.FormatUint(uint64(file.Fd()), 10)
}

func (session *windowsSession) wait() sessionResult {
	var state windowsWaitState
	var lifecycleErr error
	if !session.collect(&state, session.lease) {
		lifecycleErr = errors.New("external process owner exceeded its bounded transport lease")
		lifecycleErr = errors.Join(
			lifecycleErr,
			session.closeControl(),
			session.closeParent(),
			session.closeStart(),
		)
		if !session.collect(&state, session.retireGrace) {
			session.forceRetire()
			lifecycleErr = errors.Join(lifecycleErr, errors.New("external process owner exceeded its retirement lease"))
			if !session.collect(&state, windowsOwnerForcedJoinWait) {
				lifecycleErr = errors.Join(lifecycleErr, errors.New("external process-owner transports did not join after forced retirement"))
			}
		}
	}
	lifecycleErr = errors.Join(lifecycleErr, session.closeParent())
	var startErr error
	if state.statusReady && state.startReady {
		startErr = reconcileStartGate(state.status.settlement, state.start)
	} else if !state.startReady {
		startErr = errors.New("process-owner start gate did not join")
	}
	return sessionResult{
		settlement: state.status.settlement,
		start:      state.start,
		err: errors.Join(
			session.initialErr,
			lifecycleErr,
			state.status.err,
			state.waitErr,
			state.stdoutErr,
			state.stderrErr,
			state.eventErr,
			startErr,
		),
	}
}

func (session *windowsSession) collect(state *windowsWaitState, maximum time.Duration) bool {
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	for !state.complete() {
		var status <-chan windowsStatusResult
		var start <-chan startGateResult
		var wait, stdout, stderr, event <-chan error
		if !state.statusReady {
			status = session.status
		}
		if !state.startReady {
			start = session.startResult
		}
		if !state.waitReady {
			wait = session.waitResult
		}
		if !state.stdoutReady {
			stdout = session.stdoutResult
		}
		if !state.stderrReady {
			stderr = session.stderrResult
		}
		if !state.eventReady {
			event = session.eventResult
		}
		select {
		case state.status = <-status:
			state.statusReady = true
		case state.start = <-start:
			state.startReady = true
		case state.waitErr = <-wait:
			state.waitReady = true
		case state.stdoutErr = <-stdout:
			state.stdoutReady = true
		case state.stderrErr = <-stderr:
			state.stderrReady = true
		case state.eventErr = <-event:
			state.eventReady = true
		case <-deadline.C:
			return false
		}
	}
	return true
}

func (session *windowsSession) stop(control protocol.Control) error {
	if err := protocol.ValidateControl(control, session.identity); err != nil {
		return err
	}
	err := publishControl(session.controlWriter, control)
	return errors.Join(err, session.closeControl())
}

func (session *windowsSession) close() error {
	return errors.Join(session.closeControl(), session.closeParent(), session.closeStart())
}

func (session *windowsSession) closeControl() error {
	session.controlCloseOnce.Do(func() {
		session.controlCloseErr = session.controlWriter.Close()
		if errors.Is(session.controlCloseErr, os.ErrClosed) {
			session.controlCloseErr = nil
		}
	})
	return session.controlCloseErr
}

func (session *windowsSession) closeParent() error {
	session.parentCloseOnce.Do(func() {
		session.parentCloseErr = session.parentWriter.Close()
		if errors.Is(session.parentCloseErr, os.ErrClosed) {
			session.parentCloseErr = nil
		}
	})
	return session.parentCloseErr
}

func (session *windowsSession) closeStart() error {
	return errors.Join(
		closeStartEndpoint(session.startEvidence),
		closeStartEndpoint(session.startDecision),
	)
}

func (session *windowsSession) forceRetire() {
	_ = session.closeControl()
	_ = session.closeParent()
	_ = session.closeStart()
	if session.command != nil && session.command.Process != nil {
		_ = session.command.Process.Kill()
	}
	_ = session.statusReader.Close()
	_ = session.eventReader.Close()
	_ = session.stdoutReader.Close()
	_ = session.stderrReader.Close()
}
