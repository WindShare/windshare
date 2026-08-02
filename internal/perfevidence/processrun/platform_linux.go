//go:build linux

package processrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	linuxGuardianTransportMargin = 4 * time.Second
	linuxForcedJoinWait          = 2 * time.Second
)

type linuxStatusResult struct {
	settlement protocol.Settlement
	err        error
}

type linuxWaitState struct {
	status      linuxStatusResult
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

func (state *linuxWaitState) complete() bool {
	return state.statusReady && state.startReady && state.waitReady &&
		state.stdoutReady && state.stderrReady && state.eventReady
}

type linuxSession struct {
	command       *exec.Cmd
	identity      protocol.Identity
	controlWriter *os.File
	statusReader  *os.File
	eventReader   *os.File
	stdoutReader  io.ReadCloser
	stderrReader  io.ReadCloser
	startEvidence *os.File
	startDecision *os.File
	status        <-chan linuxStatusResult
	startResult   <-chan startGateResult
	waitResult    <-chan error
	stdoutResult  <-chan error
	stderrResult  <-chan error
	eventResult   <-chan error
	initialErr    error
	lifecycleEnd  time.Time
	retireBudget  time.Duration

	controlCloseOnce sync.Once
	controlCloseErr  error
	closeOnce        sync.Once
	closeErr         error
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
	started := time.Now()
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create process-owner status pipe: %w", err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		return nil, fmt.Errorf("create process-owner control pipe: %w", err)
	}
	inputReader, inputWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		return nil, fmt.Errorf("create process-owner input pipe: %w", err)
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner event pipe: %w", err),
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
			inputReader.Close(), inputWriter.Close())
	}
	startEvidenceReader, startEvidenceWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner start-evidence pipe: %w", err),
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
			inputReader.Close(), inputWriter.Close(), eventReader.Close(), eventWriter.Close())
	}
	startDecisionReader, startDecisionWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner start-decision pipe: %w", err),
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
			inputReader.Close(), inputWriter.Close(), eventReader.Close(), eventWriter.Close(),
			startEvidenceReader.Close(), startEvidenceWriter.Close())
	}
	closeAll := func() error {
		return errors.Join(
			statusReader.Close(), statusWriter.Close(), controlReader.Close(), controlWriter.Close(),
			inputReader.Close(), inputWriter.Close(), eventReader.Close(), eventWriter.Close(),
			startEvidenceReader.Close(), startEvidenceWriter.Close(),
			startDecisionReader.Close(), startDecisionWriter.Close(),
		)
	}
	encodedRequest, err := protocol.EncodeCanonical(request)
	if err != nil {
		return nil, errors.Join(err, closeAll())
	}

	command := exec.Command(helperPath, "guard")
	command.Dir = filepath.Dir(helperPath)
	command.Env = ownerHelperEnvironment(os.Environ())
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin = bytes.NewReader(encodedRequest)
	command.ExtraFiles = []*os.File{
		statusWriter,
		controlReader,
		inputReader,
		eventWriter,
		startEvidenceWriter,
		startDecisionReader,
	}
	ownerOutput, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner stdout pipe: %w", err), closeAll())
	}
	ownerErrorOutput, err := command.StderrPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create process-owner stderr pipe: %w", err), ownerOutput.Close(), closeAll())
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("start external process owner: %w", err), ownerOutput.Close(), ownerErrorOutput.Close(), closeAll())
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start external process owner: %w", err), ownerOutput.Close(), ownerErrorOutput.Close(), closeAll())
	}
	childCloseErr := errors.Join(
		statusWriter.Close(),
		controlReader.Close(),
		inputReader.Close(),
		eventWriter.Close(),
		startEvidenceWriter.Close(),
		startDecisionReader.Close(),
	)
	// An omitted stdin authority is represented by immediate EOF on the mandatory
	// raw-input channel; no goroutine remains capable of retaining that boundary.
	inputCloseErr := inputWriter.Close()

	statusResult := make(chan linuxStatusResult, 1)
	go func() {
		defer statusReader.Close()
		settlement, readErr := protocol.ReadLineDocument[protocol.Settlement](statusReader)
		if readErr == nil {
			readErr = protocol.ValidateSettlementForRequest(settlement, request)
		}
		statusResult <- linuxStatusResult{settlement: settlement, err: readErr}
	}()
	startResult := make(chan startGateResult, 1)
	go func() {
		startResult <- completeStartGate(startEvidenceReader, startDecisionWriter, request, spec.AuthorizeStart)
	}()
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	stdoutResult := make(chan error, 1)
	go func() { stdoutResult <- drainOutput(ownerOutput, output.stdout, "owned stdout") }()
	stderrResult := make(chan error, 1)
	go func() { stderrResult <- drainOutput(ownerErrorOutput, output.stderr, "owned stderr") }()
	eventResult := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(io.Discard, eventReader)
		eventResult <- errors.Join(copyErr, eventReader.Close())
	}()

	lifecycleEnd := started.Add(
		time.Duration(request.DeadlineMilliseconds+2*request.TerminationGraceMilliseconds)*time.Millisecond +
			linuxGuardianTransportMargin,
	)
	return &linuxSession{
		command: command, identity: request.Identity, controlWriter: controlWriter,
		statusReader: statusReader, eventReader: eventReader,
		stdoutReader: ownerOutput, stderrReader: ownerErrorOutput,
		startEvidence: startEvidenceReader, startDecision: startDecisionWriter,
		status: statusResult, startResult: startResult, waitResult: waitResult,
		stdoutResult: stdoutResult, stderrResult: stderrResult, eventResult: eventResult,
		initialErr:   errors.Join(childCloseErr, inputCloseErr),
		lifecycleEnd: lifecycleEnd,
		retireBudget: 2*time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond +
			linuxGuardianTransportMargin,
	}, nil
}

func (session *linuxSession) wait() sessionResult {
	var state linuxWaitState
	timer := time.NewTimer(max(time.Until(session.lifecycleEnd), 0))
	defer timer.Stop()
	retiring := false
	var lifecycleErr error
	for !state.complete() {
		if session.collectLinux(&state, timer.C) {
			break
		}
		if !retiring {
			lifecycleErr = errors.New("external process owner exceeded its bounded lifecycle lease")
			_ = session.close()
			retiring = true
			timer.Reset(session.retireBudget)
			continue
		}
		lifecycleErr = errors.Join(lifecycleErr, errors.New("external process owner exceeded its retirement lease"))
		session.forceRetire()
		forced := time.NewTimer(linuxForcedJoinWait)
		joined := session.collectLinux(&state, forced.C)
		forced.Stop()
		if !joined {
			lifecycleErr = errors.Join(lifecycleErr, errors.New("external process-owner transports did not join after forced retirement"))
		}
		break
	}
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

// collectLinux waits until all owner transports join or the supplied timer fires.
func (session *linuxSession) collectLinux(state *linuxWaitState, deadline <-chan time.Time) bool {
	for !state.complete() {
		var status <-chan linuxStatusResult
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
		case <-deadline:
			return false
		}
	}
	return true
}

func (session *linuxSession) stop(control protocol.Control) error {
	if err := protocol.ValidateControl(control, session.identity); err != nil {
		return err
	}
	err := publishControl(session.controlWriter, control)
	return errors.Join(err, session.closeControl())
}

func (session *linuxSession) closeControl() error {
	session.controlCloseOnce.Do(func() {
		session.controlCloseErr = session.controlWriter.Close()
	})
	return session.controlCloseErr
}

func (session *linuxSession) close() error {
	session.closeOnce.Do(func() {
		// Start authorization remains independently joinable after a stop trigger.
		// Retirement is the only path allowed to revoke that authentication channel.
		session.closeErr = errors.Join(
			session.closeControl(),
			closeStartEndpoint(session.startEvidence),
			closeStartEndpoint(session.startDecision),
		)
	})
	return session.closeErr
}

func (session *linuxSession) forceRetire() {
	if session.command != nil && session.command.Process != nil {
		_ = syscall.Kill(-session.command.Process.Pid, syscall.SIGKILL)
		_ = session.command.Process.Kill()
	}
	_ = session.statusReader.Close()
	_ = session.eventReader.Close()
	_ = session.stdoutReader.Close()
	_ = session.stderrReader.Close()
	_ = closeStartEndpoint(session.startEvidence)
	_ = closeStartEndpoint(session.startDecision)
}
