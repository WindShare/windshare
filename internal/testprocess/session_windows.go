//go:build windows

package testprocess

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const windowsOwnedOutputBufferBytes = 32 << 10

type windowsStatusResult struct {
	settlement protocol.Settlement
	err        error
}

type windowsSession struct {
	command       *exec.Cmd
	stdout        <-chan error
	stderr        <-chan error
	status        <-chan windowsStatusResult
	waitResult    <-chan error
	inputResult   <-chan error
	startResult   <-chan startGateResult
	controlWriter *os.File
	eventReader   *os.File
	parentWriter  *os.File
	inputWriter   *os.File
	statusReader  *os.File
	stdoutReader  io.ReadCloser
	stderrReader  io.ReadCloser
	startEvidence *os.File
	startDecision *os.File
	identity      protocol.Identity
	initialErr    error
	lease         time.Duration
	retireGrace   time.Duration

	controlCloseOnce sync.Once
	controlCloseErr  error
	parentCloseOnce  sync.Once
	parentCloseErr   error
	inputCloseOnce   sync.Once
	inputCloseErr    error
}

func readWindowsSettlement(
	reader *os.File,
	request protocol.Request,
	result chan<- windowsStatusResult,
) {
	defer reader.Close()
	settlement, err := protocol.ReadLineDocument[protocol.Settlement](reader)
	if err == nil {
		err = protocol.ValidateSettlementForRequest(settlement, request)
	}
	result <- windowsStatusResult{settlement: settlement, err: err}
}

func drainOwnedOutput(source io.Reader, destination *boundedOutput, label string) error {
	buffer := make([]byte, windowsOwnedOutputBufferBytes)
	var destinationErr error
	for {
		count, readErr := source.Read(buffer)
		if count > 0 && destinationErr == nil {
			written, writeErr := destination.Write(buffer[:count])
			if writeErr == nil && written != count {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				// Capture saturation is diagnostic evidence, not authority to stop
				// draining a process-owned pipe and perturb the target lifecycle.
				destinationErr = fmt.Errorf("%s capture: %w", label, writeErr)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				captureErr := destination.terminalError()
				if captureErr != nil {
					captureErr = fmt.Errorf("%s capture: %w", label, captureErr)
				}
				return errors.Join(destinationErr, captureErr)
			}
			return errors.Join(destinationErr, fmt.Errorf("drain %s: %w", label, readErr))
		}
		if count == 0 {
			return errors.Join(destinationErr, fmt.Errorf("drain %s: %w", label, io.ErrNoProgress))
		}
	}
}

type windowsWaitState struct {
	status      windowsStatusResult
	statusReady bool
	inputErr    error
	inputReady  bool
	stdoutErr   error
	stdoutReady bool
	stderrErr   error
	stderrReady bool
	waitErr     error
	waitReady   bool
	start       startGateResult
	startReady  bool
}

func (state *windowsWaitState) complete() bool {
	return state.statusReady && state.inputReady && state.stdoutReady && state.stderrReady &&
		state.waitReady && state.startReady
}

func (state *windowsWaitState) collect(
	status <-chan windowsStatusResult,
	input <-chan error,
	stdout <-chan error,
	stderr <-chan error,
	wait <-chan error,
	start <-chan startGateResult,
	maximum time.Duration,
) bool {
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	for !state.complete() {
		var statusChannel <-chan windowsStatusResult
		if !state.statusReady {
			statusChannel = status
		}
		var inputChannel, stdoutChannel, stderrChannel, waitChannel <-chan error
		var startChannel <-chan startGateResult
		if !state.inputReady {
			inputChannel = input
		}
		if !state.stdoutReady {
			stdoutChannel = stdout
		}
		if !state.stderrReady {
			stderrChannel = stderr
		}
		if !state.waitReady {
			waitChannel = wait
		}
		if !state.startReady {
			startChannel = start
		}
		select {
		case state.status = <-statusChannel:
			state.statusReady = true
		case state.inputErr = <-inputChannel:
			state.inputReady = true
		case state.stdoutErr = <-stdoutChannel:
			state.stdoutReady = true
		case state.stderrErr = <-stderrChannel:
			state.stderrReady = true
		case state.waitErr = <-waitChannel:
			state.waitReady = true
		case state.start = <-startChannel:
			state.startReady = true
		case <-deadline.C:
			return false
		}
	}
	return true
}

func (session *windowsSession) wait() (protocol.Settlement, error) {
	var state windowsWaitState
	var lifecycleErr error
	if !state.collect(
		session.status,
		session.inputResult,
		session.stdout,
		session.stderr,
		session.waitResult,
		session.startResult,
		session.lease,
	) {
		lifecycleErr = errors.New("external process owner exceeded its bounded transport lease")
		lifecycleErr = errors.Join(
			lifecycleErr,
			session.closeControl(),
			session.closeParent(),
			session.closeInput(),
			session.closeStart(),
		)
		if !state.collect(
			session.status,
			session.inputResult,
			session.stdout,
			session.stderr,
			session.waitResult,
			session.startResult,
			session.retireGrace,
		) {
			// Terminating the Windows owner closes its kill-on-close Job handle, so
			// this fallback retires the whole target tree rather than only the helper.
			killErr := session.command.Process.Kill()
			_ = session.statusReader.Close()
			_ = session.stdoutReader.Close()
			_ = session.stderrReader.Close()
			_ = session.closeStart()
			lifecycleErr = errors.Join(lifecycleErr, killErr)
			if !state.collect(
				session.status,
				session.inputResult,
				session.stdout,
				session.stderr,
				session.waitResult,
				session.startResult,
				windowsOwnerForcedJoinWait,
			) {
				lifecycleErr = errors.Join(lifecycleErr, errors.New("external process-owner transport did not join after forced retirement"))
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
	transportErr := errors.Join(
		session.initialErr,
		state.inputErr,
		state.stdoutErr,
		state.stderrErr,
		state.waitErr,
		startErr,
		lifecycleErr,
	)
	if !state.statusReady {
		return protocol.Settlement{}, errors.Join(
			errors.New("process-owner settlement stream did not join"),
			transportErr,
		)
	}
	if state.status.err != nil {
		return protocol.Settlement{}, errors.Join(
			fmt.Errorf("read process-owner settlement: %w", state.status.err),
			transportErr,
		)
	}
	if transportErr != nil {
		return state.status.settlement, fmt.Errorf("external process owner failed after settlement: %w", transportErr)
	}
	return state.status.settlement, nil
}

func (session *windowsSession) events() io.ReadCloser { return session.eventReader }

func (session *windowsSession) close() error {
	return errors.Join(session.closeControl(), session.closeParent(), session.closeInput(), session.closeStart())
}

func (session *windowsSession) closeStart() error {
	return errors.Join(
		closeStartEndpoint(session.startEvidence),
		closeStartEndpoint(session.startDecision),
	)
}

func (session *windowsSession) closeControl() error {
	session.controlCloseOnce.Do(func() { session.controlCloseErr = session.controlWriter.Close() })
	return session.controlCloseErr
}

func (session *windowsSession) closeParent() error {
	session.parentCloseOnce.Do(func() { session.parentCloseErr = session.parentWriter.Close() })
	return session.parentCloseErr
}

func (session *windowsSession) closeInput() error {
	if session.inputWriter == nil {
		return nil
	}
	session.inputCloseOnce.Do(func() {
		session.inputCloseErr = session.inputWriter.Close()
		if errors.Is(session.inputCloseErr, os.ErrClosed) {
			session.inputCloseErr = nil
		}
	})
	return session.inputCloseErr
}

func (session *windowsSession) stop(control protocol.Control) error {
	if err := protocol.ValidateControl(control, session.identity); err != nil {
		return err
	}
	if err := publishControl(session.controlWriter, control); err != nil {
		if !retryableControlPublication(err) {
			_ = session.closeControl()
		}
		return err
	}
	// A complete frame is the authoritative stop publication. Closing the pipe
	// only fences trailing bytes and remains terminal cleanup evidence.
	_ = session.closeControl()
	return nil
}
