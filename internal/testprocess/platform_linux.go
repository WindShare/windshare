//go:build linux

package testprocess

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

const linuxGuardianTransportMargin = 4 * time.Second

type linuxStatusResult struct {
	settlement protocol.Settlement
	err        error
}

type linuxInputResult struct {
	written int64
	err     error
}

type linuxSession struct {
	command        *exec.Cmd
	controlWriter  *os.File
	statusReader   *os.File
	eventReader    *os.File
	output         *processOutput
	inputWriter    *os.File
	identity       protocol.Identity
	status         <-chan linuxStatusResult
	waitResult     <-chan error
	inputResult    <-chan linuxInputResult
	startResult    <-chan startGateResult
	startEvidence  *os.File
	startDecision  *os.File
	lifecycleEnd   time.Time
	retireBudget   time.Duration
	closeOnce      sync.Once
	closeErr       error
	inputCloseOnce sync.Once
	inputCloseErr  error
}

func startPlatform(
	ctx context.Context,
	helperPath string,
	spec Spec,
	request protocol.Request,
	output *processOutput,
) (platformSession, error) {
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start external process owner: %w", err)
	}
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
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		return nil, fmt.Errorf("create private test-event pipe: %w", err)
	}
	startEvidenceReader, startEvidenceWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
		return nil, fmt.Errorf("create process-owner start-evidence pipe: %w", err)
	}
	startDecisionReader, startDecisionWriter, err := os.Pipe()
	if err != nil {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
		_ = startEvidenceReader.Close()
		_ = startEvidenceWriter.Close()
		return nil, fmt.Errorf("create process-owner start-decision pipe: %w", err)
	}
	closeAll := func() {
		_ = statusReader.Close()
		_ = statusWriter.Close()
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = inputReader.Close()
		_ = inputWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
		_ = startEvidenceReader.Close()
		_ = startEvidenceWriter.Close()
		_ = startDecisionReader.Close()
		_ = startDecisionWriter.Close()
	}
	encodedRequest, err := protocol.EncodeCanonical(request)
	if err != nil {
		closeAll()
		return nil, err
	}
	lifecycleEnd := started.Add(
		time.Duration(request.DeadlineMilliseconds+2*request.TerminationGraceMilliseconds)*time.Millisecond +
			linuxGuardianTransportMargin,
	)
	retireBudget := 2*time.Duration(request.TerminationGraceMilliseconds)*time.Millisecond +
		linuxGuardianTransportMargin
	// The writer deadline makes the raw-input worker intrinsically joinable even
	// if both owner layers violate their leases; Wait can revoke and join it
	// without leaving a transport goroutine behind.
	if err := inputWriter.SetWriteDeadline(lifecycleEnd.Add(retireBudget)); err != nil {
		closeAll()
		return nil, fmt.Errorf("bound process-owner input delivery: %w", err)
	}
	command := exec.Command(helperPath, "guard")
	configureLinuxOwnerCommand(command, helperPath)
	command.Stdin = bytes.NewReader(encodedRequest)
	command.Stdout = output.stdout
	command.Stderr = output.stderr
	command.ExtraFiles = []*os.File{
		statusWriter,
		controlReader,
		inputReader,
		eventWriter,
		startEvidenceWriter,
		startDecisionReader,
	}
	if err := ctx.Err(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start external process owner: %w", err)
	}
	if err := command.Start(); err != nil {
		closeAll()
		return nil, fmt.Errorf("start external process owner: %w", err)
	}
	_ = statusWriter.Close()
	_ = controlReader.Close()
	_ = inputReader.Close()
	_ = eventWriter.Close()
	_ = startEvidenceWriter.Close()
	_ = startDecisionReader.Close()
	inputResult := make(chan linuxInputResult, 1)
	statusResult := make(chan linuxStatusResult, 1)
	go func() {
		defer statusReader.Close()
		settlement, readErr := protocol.ReadLineDocument[protocol.Settlement](statusReader)
		if readErr == nil {
			readErr = protocol.ValidateSettlementForRequest(settlement, request)
		}
		statusResult <- linuxStatusResult{settlement: settlement, err: readErr}
	}()
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	startResult := make(chan startGateResult, 1)
	go func() {
		startResult <- completeStartGate(startEvidenceReader, startDecisionWriter, request)
	}()
	session := &linuxSession{
		command: command, controlWriter: controlWriter, identity: request.Identity,
		status: statusResult, waitResult: waitResult, statusReader: statusReader, eventReader: eventReader,
		output: output, inputWriter: inputWriter, inputResult: inputResult, startResult: startResult,
		startEvidence: startEvidenceReader, startDecision: startDecisionWriter,
		lifecycleEnd: lifecycleEnd,
		retireBudget: retireBudget,
	}
	go func() {
		expected := int64(len(spec.Command.Stdin))
		written, copyErr := io.Copy(inputWriter, bytes.NewReader(spec.Command.Stdin))
		if written != expected && copyErr == nil {
			copyErr = io.ErrShortWrite
		}
		closeErr := session.closeInput()
		deliveryErr := errors.Join(copyErr, closeErr)
		if deliveryErr != nil {
			deliveryErr = fmt.Errorf(
				"deliver exact process-owner input: wrote %d of %d bytes: %w",
				written,
				expected,
				deliveryErr,
			)
		}
		inputResult <- linuxInputResult{
			written: written,
			err:     deliveryErr,
		}
	}()
	return session, nil
}

func configureLinuxOwnerCommand(command *exec.Cmd, helperPath string) {
	// The guardian must outlive a terminal signal sent to the runner's process
	// group so control-pipe EOF can trigger authoritative tree retirement.
	command.Dir = filepath.Dir(helperPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func (session *linuxSession) wait() (protocol.Settlement, error) {
	statusChannel := session.status
	waitChannel := session.waitResult
	inputChannel := session.inputResult
	startChannel := session.startResult
	var status linuxStatusResult
	var waitErr error
	var input linuxInputResult
	var start startGateResult
	statusReady := false
	waitReady := false
	inputReady := inputChannel == nil
	startReady := startChannel == nil
	retiring := false
	timer := time.NewTimer(max(time.Until(session.lifecycleEnd), 0))
	defer timer.Stop()
	for !statusReady || !waitReady || !inputReady || !startReady {
		select {
		case status = <-statusChannel:
			statusReady = true
			statusChannel = nil
		case waitErr = <-waitChannel:
			waitReady = true
			waitChannel = nil
		case input = <-inputChannel:
			inputReady = true
			inputChannel = nil
		case start = <-startChannel:
			startReady = true
			startChannel = nil
		case <-timer.C:
			// Prefer already-published terminal evidence when it raced the lease.
			if !statusReady {
				select {
				case status = <-statusChannel:
					statusReady = true
					statusChannel = nil
				default:
				}
			}
			if !waitReady {
				select {
				case waitErr = <-waitChannel:
					waitReady = true
					waitChannel = nil
				default:
				}
			}
			if !inputReady {
				select {
				case input = <-inputChannel:
					inputReady = true
					inputChannel = nil
				default:
				}
			}
			if !startReady {
				select {
				case start = <-startChannel:
					startReady = true
					startChannel = nil
				default:
				}
			}
			if statusReady && waitReady && inputReady && startReady {
				continue
			}
			if !retiring {
				// EOF is the only client-side escalation: it preserves the external
				// supervisor's subreaper and pidfd authority over the target tree.
				_ = session.close()
				retiring = true
				timer.Reset(session.retireBudget)
				continue
			}
			if !statusReady {
				_ = session.statusReader.Close()
			}
			_ = session.closeInput()
			if !inputReady {
				input = <-inputChannel
			}
			if !startReady {
				start = <-startChannel
			}
			return status.settlement, errors.Join(
				status.err,
				errors.New("external process owner exceeded its bounded lifecycle and retirement leases"),
				input.err,
				reconcileStartGate(status.settlement, start),
			)
		}
	}
	outputErr := processOutputTerminalError(session.output)
	inputErr := input.err
	startErr := reconcileStartGate(status.settlement, start)
	if status.err != nil {
		return protocol.Settlement{}, errors.Join(
			fmt.Errorf("read process-owner settlement: %w", status.err),
			waitErr,
			inputErr,
			startErr,
			outputErr,
		)
	}
	if waitErr != nil {
		return status.settlement, errors.Join(
			fmt.Errorf("external process owner failed after settlement: %w", waitErr),
			inputErr,
			startErr,
			outputErr,
		)
	}
	return status.settlement, errors.Join(inputErr, startErr, outputErr)
}

func processOutputTerminalError(output *processOutput) error {
	if output == nil {
		return nil
	}
	return errors.Join(output.stdout.terminalError(), output.stderr.terminalError())
}

func (session *linuxSession) events() io.ReadCloser { return session.eventReader }

func (session *linuxSession) close() error {
	session.closeOnce.Do(func() {
		session.closeErr = errors.Join(
			session.controlWriter.Close(),
			session.closeInput(),
			closeStartEndpoint(session.startEvidence),
			closeStartEndpoint(session.startDecision),
		)
	})
	return session.closeErr
}

func (session *linuxSession) closeInput() error {
	if session == nil || session.inputWriter == nil {
		return nil
	}
	session.inputCloseOnce.Do(func() { session.inputCloseErr = session.inputWriter.Close() })
	return session.inputCloseErr
}

func (session *linuxSession) stop(control protocol.Control) error {
	if err := protocol.ValidateControl(control, session.identity); err != nil {
		return err
	}
	if err := publishControl(session.controlWriter, control); err != nil {
		if !retryableControlPublication(err) {
			_ = session.close()
		}
		return err
	}
	// A complete frame is the authoritative stop publication. A later close
	// failure is retained for Wait, but must not make this issued stop retryable.
	_ = session.close()
	return nil
}
