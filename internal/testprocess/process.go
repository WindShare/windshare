package testprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/processowner"
)

const (
	helperStartupTimeout  = 10 * time.Second
	helperSettlementGrace = 5 * time.Second
	helperPipeDrainGrace  = 2 * time.Second
)

var errReadinessBeforeExit = errors.New("process exited before readiness")

type Result = processowner.Result

type Process struct {
	identity string
	stdout   *boundedOutput
	stderr   *boundedOutput
	events   *EventReader
	control  io.WriteCloser
	done     chan struct{}

	mu           sync.Mutex
	result       Result
	lifecycleErr error
	controlSent  bool
}

type startupResult struct {
	status processowner.Status
	err    error
}

func startProcess(
	ctx context.Context,
	helperPath string,
	spec Spec,
	released func(),
) (_ *Process, resultErr error) {
	config, err := configForSpec(spec)
	if err != nil {
		return nil, err
	}
	encoded, err := processowner.EncodeConfig(config)
	if err != nil {
		return nil, err
	}
	lifetimeContext, cancelLifetime := context.WithTimeout(
		context.Background(),
		spec.Deadline+spec.TerminationGrace+helperSettlementGrace,
	)
	platform, err := newPlatformCommand(lifetimeContext, helperPath, config.WorkingDirectory, encoded)
	if err != nil {
		cancelLifetime()
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			cancelLifetime()
			resultErr = errors.Join(resultErr, platform.closeAll())
		}
	}()
	stdoutPipe, err := platform.command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := platform.command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := platform.command.Start(); err != nil {
		return nil, fmt.Errorf("start external process owner: %w", err)
	}
	childCloseErr := platform.closeChildEnds()
	stdout := newBoundedOutput()
	stderr := newBoundedOutput()
	stdoutDone := drainOutput(stdoutPipe, stdout)
	stderrDone := drainOutput(stderrPipe, stderr)
	events := newEventReader(platform.events)
	decoder := processowner.NewStatusDecoder(platform.status)
	startup := make(chan startupResult, 1)
	go func() {
		status, decodeErr := decoder.Next()
		startup <- startupResult{status: status, err: decodeErr}
	}()
	startupTimer := time.NewTimer(helperStartupTimeout)
	defer startupTimer.Stop()
	var first startupResult
	select {
	case first = <-startup:
	case <-ctx.Done():
		_ = platform.command.Process.Kill()
		_ = platform.command.Wait()
		return nil, fmt.Errorf("start process owner: %w", ctx.Err())
	case <-startupTimer.C:
		_ = platform.command.Process.Kill()
		_ = platform.command.Wait()
		return nil, errors.New("process owner did not report startup within its bound")
	}
	if first.err != nil {
		_ = platform.command.Process.Kill()
		waitErr := platform.command.Wait()
		return nil, errors.Join(first.err, waitErr, <-stdoutDone, <-stderrDone, childCloseErr)
	}
	if first.status.State == processowner.StatusFinished {
		waitErr := platform.command.Wait()
		result := first.status.Result
		return nil, errors.Join(
			fmt.Errorf("start owned process: %s", result.Error),
			waitErr, <-stdoutDone, <-stderrDone, childCloseErr,
		)
	}
	process := &Process{
		identity: spec.Identity.OperationID,
		stdout:   stdout, stderr: stderr, events: events,
		control: platform.control, done: make(chan struct{}),
	}
	started = true
	go process.finish(platform, decoder, stdoutDone, stderrDone, childCloseErr, released, cancelLifetime)
	return process, nil
}

func drainOutput(source io.Reader, destination io.Writer) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(destination, source)
		done <- err
	}()
	return done
}

func (process *Process) finish(
	platform *platformCommand,
	decoder *processowner.StatusDecoder,
	stdoutDone, stderrDone <-chan error,
	initialErr error,
	released func(),
	cancelLifetime context.CancelFunc,
) {
	defer close(process.done)
	defer released()
	defer cancelLifetime()
	status, statusErr := decoder.Next()
	var result Result
	if statusErr == nil {
		if status.State != processowner.StatusFinished || status.Result == nil {
			statusErr = errors.New("process owner omitted its terminal result")
		} else {
			result = cloneResult(*status.Result)
		}
	}
	waitErr := platform.command.Wait()
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) && statusErr == nil {
		waitErr = fmt.Errorf("process owner exited with %d", exitError.ExitCode())
	}
	eventJoinErr := waitForEventReader(process.events, platform.events, helperPipeDrainGrace)
	closeErr := errors.Join(platform.status.Close(), platform.control.Close())
	process.mu.Lock()
	process.result = result
	process.lifecycleErr = errors.Join(
		initialErr,
		statusErr,
		waitErr,
		<-stdoutDone,
		<-stderrDone,
		eventJoinErr,
		closeErr,
	)
	process.mu.Unlock()
}

func waitForEventReader(reader *EventReader, source io.Closer, maximum time.Duration) error {
	timer := time.NewTimer(maximum)
	defer timer.Stop()
	select {
	case <-reader.Done():
		return reader.Err()
	case <-timer.C:
		// A target that leaks the event descriptor must not keep the process owner
		// or its test runner alive after the bounded lifecycle has settled.
		closeErr := source.Close()
		if errors.Is(closeErr, os.ErrClosed) {
			closeErr = nil
		}
		return errors.Join(errors.New("test process event reader did not settle"), closeErr)
	}
}

func (process *Process) Stdout() OutputSnapshot { return process.stdout.snapshot() }

func (process *Process) Stderr() OutputSnapshot { return process.stderr.snapshot() }

func (process *Process) Events() *EventReader { return process.events }

func (process *Process) WaitForOutput(
	ctx context.Context,
	stream OutputStream,
	pattern *regexp.Regexp,
) ([]string, error) {
	if pattern == nil {
		return nil, errors.New("readiness pattern is nil")
	}
	var output *boundedOutput
	switch stream {
	case Stdout:
		output = process.stdout
	case Stderr:
		output = process.stderr
	default:
		return nil, fmt.Errorf("unsupported process output stream %q", stream)
	}
	match, err := output.waitFor(ctx, process.done, pattern)
	if err != nil {
		return nil, fmt.Errorf("wait for %s readiness: %w; %s", stream, err, process.Diagnostic())
	}
	return match, nil
}

func (process *Process) Wait(ctx context.Context) (Result, error) {
	select {
	case <-process.done:
		return process.terminal()
	case <-ctx.Done():
		_ = process.sendControl(processowner.ControlStop)
		return Result{}, ctx.Err()
	}
}

func (process *Process) Stop(ctx context.Context) (Result, error) {
	if err := process.sendControl(processowner.ControlStop); err != nil {
		return Result{}, err
	}
	return process.Wait(ctx)
}

func (process *Process) Interrupt(ctx context.Context) (Result, error) {
	if err := process.sendControl(processowner.ControlInterrupt); err != nil {
		return Result{}, err
	}
	return process.Wait(ctx)
}

func (process *Process) sendControl(command byte) error {
	select {
	case <-process.done:
		return nil
	default:
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.controlSent {
		return nil
	}
	if _, err := process.control.Write([]byte{command}); err != nil {
		return fmt.Errorf("send process control: %w", err)
	}
	process.controlSent = true
	return nil
}

func (process *Process) terminal() (Result, error) {
	process.mu.Lock()
	defer process.mu.Unlock()
	return cloneResult(process.result), process.lifecycleErr
}

func (process *Process) Diagnostic() string {
	result, err := process.terminalIfReady()
	return fmt.Sprintf(
		"operation_id=%s exit_code=%v reason=%s lifecycle_error=%v stdout=%q stderr=%q",
		process.identity,
		result.ExitCode,
		result.Reason,
		err,
		process.Stdout().String(),
		process.Stderr().String(),
	)
}

func (process *Process) terminalIfReady() (Result, error) {
	select {
	case <-process.done:
		return process.terminal()
	default:
		return Result{}, nil
	}
}

func cloneResult(result Result) Result {
	if result.ExitCode != nil {
		exitCode := *result.ExitCode
		result.ExitCode = &exitCode
	}
	return result
}

func RequireSuccess(result Result, lifecycleErr error) error {
	if err := RequireClean(result, lifecycleErr); err != nil {
		return err
	}
	if *result.ExitCode != 0 {
		return fmt.Errorf("owned process exited with code %d", *result.ExitCode)
	}
	return nil
}

// RequireClean separates process-tree ownership from the target's business
// exit status. Cleanup paths use it because an intentional stop may validly
// produce a non-zero target exit while still retiring every descendant.
func RequireClean(result Result, lifecycleErr error) error {
	if lifecycleErr != nil {
		return fmt.Errorf("owned process lifecycle failed: %w", lifecycleErr)
	}
	if result.Error != "" {
		return fmt.Errorf("owned process failed: %s", result.Error)
	}
	if result.CleanupError != "" {
		return fmt.Errorf("owned process cleanup failed: %s", result.CleanupError)
	}
	if result.ExitCode == nil {
		return fmt.Errorf("owned process exited with code %v", result.ExitCode)
	}
	return nil
}
