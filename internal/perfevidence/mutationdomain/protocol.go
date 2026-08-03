package mutationdomain

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
	mutationwire "github.com/windshare/windshare/internal/perfevidence/mutationdomain/wire"
)

const (
	helperArgument            = "--perfevidence-mutation-helper"
	helperRoleEnvironment     = "WINDSHARE_PERFEVIDENCE_MUTATION_HELPER"
	maximumProtocolLine       = mutationwire.MaximumProtocolLine
	maximumCapturedBytes      = mutationwire.MaximumCapturedBytes
	privateInputDirectory     = "inputs"
	privateOutputDirectory    = "outputs"
	privateCacheDirectory     = "build-cache"
	privateTemporaryDirectory = "temporary"
	privatePromotedDirectory  = "promoted"
	sessionShutdownTimeout    = 5 * time.Second
	maximumSinkOperationTime  = 5 * time.Minute
	captureSettlementTimeout  = 5 * time.Second
	targetStartedEvent        = mutationwire.TargetStartedEvent
	targetFinishedEvent       = mutationwire.TargetFinishedEvent
	targetSettledEvent        = mutationwire.TargetSettledEvent
)

type initialization = mutationwire.Initialization
type rootSpec = mutationwire.RootSpec
type request = mutationwire.Request
type frame = mutationwire.Frame
type response = mutationwire.Response

type limitedBuffer struct {
	mu      sync.Mutex
	limit   int
	capture *mutationwire.BoundedCapture
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	buffer.mu.Lock()
	if buffer.capture == nil {
		buffer.capture = mutationwire.NewBoundedCapture(buffer.limit)
	}
	capture := buffer.capture
	buffer.mu.Unlock()
	return capture.Write(content)
}

func (buffer *limitedBuffer) snapshot() []byte {
	buffer.mu.Lock()
	capture := buffer.capture
	buffer.mu.Unlock()
	if capture == nil {
		return nil
	}
	return capture.Snapshot()
}

func (buffer *limitedBuffer) exceeded() bool {
	buffer.mu.Lock()
	capture := buffer.capture
	buffer.mu.Unlock()
	if capture == nil {
		return false
	}
	return capture.Exceeded()
}

type session struct {
	stateMu          sync.Mutex
	runGate          chan struct{}
	closedSignal     chan struct{}
	runsSettled      chan struct{}
	registeredRuns   int
	stdin            io.WriteCloser
	stdout           *bufio.Reader
	stdoutPipe       io.ReadCloser
	stderr           *limitedBuffer
	kill             func() error
	wait             func() error
	closePlatform    func() error
	resolveProcessID func(int) (int, error)
	closed           bool
	closeOnce        sync.Once
	closeErr         error
	terminateOnce    sync.Once
	terminateDone    chan struct{}
	terminateErr     error
	shutdownAfter    time.Duration
	sinkAfter        time.Duration
}

func (session *session) Run(
	ctx context.Context,
	command perfevidence.MutationDomainCommand,
	sinks map[string]perfevidence.MutationOutputSink,
) (result perfevidence.MutationDomainResult, resultErr error) {
	closedSignal, registered := session.registerRun()
	if !registered {
		return perfevidence.MutationDomainResult{ExitCode: -1}, errors.New("private mutation domain is closed")
	}
	defer session.unregisterRun()
	gate := session.protocolGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return perfevidence.MutationDomainResult{ExitCode: -1}, context.Cause(ctx)
	case <-closedSignal:
		return perfevidence.MutationDomainResult{ExitCode: -1}, errors.New("private mutation domain is closed")
	}
	_, closed := session.lifecycleState()
	if closed {
		return perfevidence.MutationDomainResult{ExitCode: -1}, errors.New("private mutation domain is closed")
	}
	if err := ctx.Err(); err != nil {
		return perfevidence.MutationDomainResult{ExitCode: -1}, err
	}
	cancellationSettled := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = session.terminate(true)
		close(cancellationSettled)
	})
	defer func() {
		if stopCancellation() {
			close(cancellationSettled)
		} else {
			<-cancellationSettled
			resultErr = errors.Join(resultErr, context.Cause(ctx))
		}
	}()
	if err := writeJSONLine(session.stdin, request{Command: command}); err != nil {
		return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "send isolated command", err)
	}
	var header response
	if err := readJSONLine(session.stdout, &header); err != nil {
		return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "read isolated command header", err)
	}
	processID := 0
	namespaceProcessID := 0
	if header.Event == targetStartedEvent {
		if header.NamespaceProcessID <= 0 || session.resolveProcessID == nil {
			return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(
				ctx, "resolve isolated target process", errors.New("isolated target start event is invalid"),
			)
		}
		resolvedID, resolveErr := session.resolveProcessID(header.NamespaceProcessID)
		namespaceProcessID = header.NamespaceProcessID
		processID = resolvedID
		if resolveErr != nil || processID <= 0 {
			return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(
				ctx, "resolve isolated target host process", errors.Join(resolveErr, fmt.Errorf("resolved process ID %d", processID)),
			)
		}
		if err := writeJSONLine(session.stdin, request{ProcessIDAcknowledged: true}); err != nil {
			return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "acknowledge isolated target process", err)
		}
		if err := readJSONLine(session.stdout, &header); err != nil {
			return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "read isolated command completion", err)
		}
	}
	if header.Event != targetFinishedEvent || (header.Fatal && header.Error == "") ||
		(processID > 0 && header.NamespaceProcessID != namespaceProcessID) ||
		(processID == 0 && header.NamespaceProcessID != 0) {
		return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(
			ctx, "validate isolated command lifecycle", errors.New("isolated target completion event is invalid"),
		)
	}
	if len(header.Frames) < 2 || len(header.Frames) > 2+len(command.Outputs) {
		err := errors.New("isolated command returned an invalid frame count")
		return perfevidence.MutationDomainResult{ExitCode: -1}, errors.Join(err, session.terminate(true))
	}
	stdout, err := readBoundedFrame(session.stdout, header.Frames[0], maximumCapturedBytes, nil)
	if err != nil {
		return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "read isolated stdout", err)
	}
	stderr, err := readBoundedFrame(session.stdout, header.Frames[1], maximumCapturedBytes, nil)
	if err != nil {
		return perfevidence.MutationDomainResult{ExitCode: -1}, session.protocolFailure(ctx, "read isolated stderr", err)
	}
	result = perfevidence.MutationDomainResult{
		Stdout: stdout, Stderr: stderr, ProcessID: processID, ExitCode: header.ExitCode,
		StartedAt: header.StartedAt, FinishedAt: header.FinishedAt,
	}
	type pendingSeal struct {
		output      perfevidence.MutationOutput
		description frame
		sink        perfevidence.MutationOutputSink
		ctx         context.Context
		settle      func()
	}
	pending := make([]pendingSeal, 0, len(header.Frames)-2)
	defer func() {
		for _, commit := range pending {
			commit.settle()
		}
	}()
	for index := 2; index < len(header.Frames); index++ {
		outputFrame := header.Frames[index]
		var output perfevidence.MutationOutput
		found := false
		for _, candidate := range command.Outputs {
			if candidate.HostPath == outputFrame.Name {
				output = candidate
				found = true
				break
			}
		}
		if !found {
			return result, errors.Join(
				fmt.Errorf("isolated command returned unknown output frame %s", outputFrame.Name), session.terminate(true),
			)
		}
		sink := sinks[output.HostPath]
		if sink == nil {
			return result, errors.Join(
				fmt.Errorf("isolated output %s has no retained sink", output.HostPath), session.terminate(true),
			)
		}
		sinkContext, settleSink := session.sinkContext(ctx, closedSignal)
		pending = append(pending, pendingSeal{
			output: output, description: outputFrame, sink: sink, ctx: sinkContext, settle: settleSink,
		})
		if _, err := readBoundedFrame(
			session.stdout, outputFrame, output.MaxBytes,
			&contextSinkWriter{ctx: sinkContext, sink: sink},
		); err != nil {
			return result, errors.Join(
				fmt.Errorf("receive isolated output %s: %w", output.HostPath, err), session.terminate(true),
			)
		}
	}
	var trailer response
	if err := readJSONLine(session.stdout, &trailer); err != nil {
		return result, session.protocolFailure(ctx, "read isolated output settlement", err)
	}
	if trailer.Event != targetSettledEvent || trailer.ExitCode != header.ExitCode || len(trailer.Frames) != 0 ||
		(trailer.Fatal && trailer.Error == "") {
		return result, session.protocolFailure(
			ctx, "validate isolated output settlement", errors.New("isolated output settlement event is invalid"),
		)
	}
	if header.Fatal || trailer.Fatal {
		return result, errors.Join(
			protocolResponseError(header.Error), protocolResponseError(trailer.Error), session.terminate(true),
		)
	}
	if header.Error != "" || trailer.Error != "" {
		return result, errors.Join(protocolResponseError(header.Error), protocolResponseError(trailer.Error))
	}
	if len(header.Frames) != 2+len(command.Outputs) || len(pending) != len(command.Outputs) {
		return result, errors.Join(
			errors.New("successful isolated command omitted a protected output frame"), session.terminate(true),
		)
	}
	for index, output := range command.Outputs {
		seal := pending[index]
		if seal.output.HostPath != output.HostPath {
			return result, errors.Join(
				fmt.Errorf("isolated output frame %q does not match %q", seal.output.HostPath, output.HostPath),
				session.terminate(true),
			)
		}
		if err := seal.sink.Seal(seal.ctx, seal.description.Bytes, seal.description.SHA256); err != nil {
			return result, errors.Join(
				fmt.Errorf("seal isolated output %s: %w", output.HostPath, err), session.terminate(true),
			)
		}
	}
	return result, nil
}

func (session *session) Close() error {
	if session == nil {
		return nil
	}
	session.closeOnce.Do(func() { session.closeErr = session.closeSession() })
	return session.closeErr
}

func (session *session) closeSession() (resultErr error) {
	session.markClosed()
	runsSettled := session.registeredRunSettlement()
	defer func() { <-runsSettled }()
	// A concurrent Run owns the protocol stream. Killing the isolated tree is
	// the only bounded close: waiting for that Run would let a hung target hold
	// the application shutdown path forever.
	gate := session.protocolGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		terminalErr := session.terminate(true)
		// Closing the protocol streams cancels any in-flight sink operation, but
		// the sink owns its durable settlement. Joining the gate proves that the
		// Run observed that cancellation and finished before Close releases the
		// session to its caller.
		return terminalErr
	}
	protocolDone := make(chan error, 1)
	go func() {
		var responseHeader response
		sendErr := writeJSONLine(session.stdin, request{Shutdown: true})
		var readErr error
		if sendErr == nil {
			readErr = readJSONLine(session.stdout, &responseHeader)
		}
		if responseHeader.Error != "" {
			readErr = errors.Join(readErr, errors.New(responseHeader.Error))
		}
		protocolDone <- errors.Join(sendErr, readErr)
	}()
	timer := time.NewTimer(session.shutdownTimeout())
	defer timer.Stop()
	select {
	case protocolErr := <-protocolDone:
		return errors.Join(protocolErr, session.terminate(protocolErr != nil))
	case <-timer.C:
		return errors.Join(errors.New("private mutation shutdown exceeded its bound"), session.terminate(true))
	}
}

func protocolResponseError(message string) error {
	if message == "" {
		return nil
	}
	return errors.New(message)
}

func (session *session) protocolGate() chan struct{} {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.runGate == nil {
		session.runGate = make(chan struct{}, 1)
	}
	return session.runGate
}

func (session *session) lifecycleState() (<-chan struct{}, bool) {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.closedSignal == nil {
		session.closedSignal = make(chan struct{})
	}
	return session.closedSignal, session.closed
}

func (session *session) registerRun() (<-chan struct{}, bool) {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.closedSignal == nil {
		session.closedSignal = make(chan struct{})
	}
	if session.closed {
		return session.closedSignal, false
	}
	session.registeredRuns++
	return session.closedSignal, true
}

func (session *session) unregisterRun() {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.registeredRuns <= 0 {
		return
	}
	session.registeredRuns--
	if session.closed && session.registeredRuns == 0 && session.runsSettled != nil {
		close(session.runsSettled)
		session.runsSettled = nil
	}
}

func (session *session) registeredRunSettlement() <-chan struct{} {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.registeredRuns == 0 {
		settled := make(chan struct{})
		close(settled)
		return settled
	}
	if session.runsSettled == nil {
		session.runsSettled = make(chan struct{})
	}
	return session.runsSettled
}

func (session *session) markClosed() bool {
	session.stateMu.Lock()
	defer session.stateMu.Unlock()
	if session.closed {
		return false
	}
	session.closed = true
	if session.closedSignal == nil {
		session.closedSignal = make(chan struct{})
	}
	close(session.closedSignal)
	if session.registeredRuns == 0 && session.runsSettled != nil {
		close(session.runsSettled)
		session.runsSettled = nil
	}
	return true
}

func (session *session) protocolFailure(ctx context.Context, operation string, operationErr error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		operationErr = errors.Join(operationErr, contextErr)
	}
	return errors.Join(fmt.Errorf("%s: %w", operation, operationErr), session.terminate(true))
}

func (session *session) terminate(kill bool) error {
	if session == nil {
		return nil
	}
	session.markClosed()
	session.terminateOnce.Do(func() {
		session.terminateDone = make(chan struct{})
		go func() {
			var killErr error
			if kill && session.kill != nil {
				killErr = session.kill()
			}
			var stdoutErr error
			if session.stdoutPipe != nil {
				stdoutErr = session.stdoutPipe.Close()
			}
			var stdinErr error
			if session.stdin != nil {
				stdinErr = session.stdin.Close()
			}
			var waitErr error
			if session.wait != nil {
				waitErr = session.wait()
			}
			var platformErr error
			if session.closePlatform != nil {
				platformErr = session.closePlatform()
			}
			session.stateMu.Lock()
			session.terminateErr = errors.Join(killErr, stdinErr, stdoutErr, waitErr, platformErr)
			close(session.terminateDone)
			session.stateMu.Unlock()
		}()
	})
	timer := time.NewTimer(session.shutdownTimeout())
	defer timer.Stop()
	select {
	case <-session.terminateDone:
		session.stateMu.Lock()
		defer session.stateMu.Unlock()
		return session.terminateErr
	case <-timer.C:
		return errors.New("private mutation process settlement exceeded its bound")
	}
}

func (session *session) shutdownTimeout() time.Duration {
	if session.shutdownAfter > 0 {
		return session.shutdownAfter
	}
	return sessionShutdownTimeout
}

func (session *session) sinkTimeout() time.Duration {
	if session.sinkAfter > 0 {
		return session.sinkAfter
	}
	return maximumSinkOperationTime
}

type contextSinkWriter struct {
	ctx  context.Context
	sink perfevidence.MutationOutputSink
}

func (writer *contextSinkWriter) Write(content []byte) (int, error) {
	return writer.sink.WriteContext(writer.ctx, content)
}

func (session *session) sinkContext(
	ctx context.Context,
	closed <-chan struct{},
) (context.Context, func()) {
	operationContext, cancel := context.WithTimeout(ctx, session.sinkTimeout())
	settled := make(chan struct{})
	go func() {
		select {
		case <-closed:
			cancel()
		case <-operationContext.Done():
		}
		close(settled)
	}()
	return operationContext, func() {
		cancel()
		<-settled
	}
}

func writeJSONLine(writer io.Writer, value any) error {
	return mutationwire.WriteJSONLine(writer, value)
}

func readJSONLine(reader *bufio.Reader, destination any) error {
	return mutationwire.ReadJSONLine(reader, destination)
}

func readBoundedFrame(
	reader io.Reader,
	description frame,
	maximum int64,
	destination io.Writer,
) ([]byte, error) {
	return mutationwire.ReadBoundedFrame(reader, description, maximum, destination)
}

func hashBytes(content []byte) string {
	return mutationwire.HashBytes(content)
}
