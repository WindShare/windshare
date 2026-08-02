package testprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	eventReaderSettlementWait  = 2 * time.Second
	eventReaderForcedCloseWait = 2 * time.Second
	stopAttemptJoinWait        = 2 * time.Second
)

type platformSession interface {
	wait() (protocol.Settlement, error)
	stop(protocol.Control) error
	close() error
	events() io.ReadCloser
}

type Process struct {
	identity protocol.Identity
	// request is retained for the lifetime of the process so terminal evidence
	// can be interpreted against the exact stdin authority that was granted.
	// A settlement alone cannot distinguish an omitted input stream from an
	// input stream that was never started.
	request *protocol.Request
	session platformSession
	done    chan struct{}

	mu           sync.Mutex
	settlement   protocol.Settlement
	waitErr      error
	resultErr    error
	stopErr      error
	terminal     bool
	resultSealed bool
	stopSent     bool
	stopActive   bool
	stopDone     chan struct{}
	events       *EventReader
	output       *processOutput
}

func newProcess(identity protocol.Identity, session platformSession, released func()) *Process {
	return newProcessWithRequest(nil, identity, session, released)
}

func newProcessWithRequest(request *protocol.Request, identity protocol.Identity, session platformSession, released func()) *Process {
	return newProcessWithOutput(request, identity, session, released, newProcessOutput())
}

func newProcessWithOutput(
	request *protocol.Request,
	identity protocol.Identity,
	session platformSession,
	released func(),
	output *processOutput,
) *Process {
	process := &Process{
		identity: identity, request: cloneRequest(request), session: session, done: make(chan struct{}),
		events: newEventReader(session.events(), identity), output: output,
	}
	go func() {
		settlement, waitErr := session.wait()
		settlement = cloneSettlement(settlement)
		if process.request != nil && (waitErr == nil || settlement.SchemaVersion != "") {
			// A secondary transport failure must not bypass request binding for a
			// settlement that was already decoded. Consumers may inspect that terminal
			// evidence alongside the error only after its identity/input contract passes.
			waitErr = errors.Join(waitErr, protocol.ValidateSettlementForRequest(settlement, *process.request))
		}
		process.mu.Lock()
		process.settlement = settlement
		process.terminal = true
		stopDone := process.stopDone
		process.mu.Unlock()
		if stopDone != nil {
			timer := time.NewTimer(stopAttemptJoinWait)
			select {
			case <-stopDone:
			case <-timer.C:
				waitErr = errors.Join(waitErr, errors.New("process-owner stop publication did not join after settlement"))
			}
			timer.Stop()
		}
		// Platform resources remain available until the terminal transition has
		// excluded new stops and any in-flight stop has completed.
		waitErr = errors.Join(waitErr, session.close())
		waitErr = errors.Join(waitErr, process.joinEventReader())
		process.mu.Lock()
		process.waitErr = waitErr
		if settlement.TerminationReason == protocol.TerminationNatural {
			process.stopErr = nil
		}
		process.resultErr = errors.Join(process.waitErr, process.stopErr)
		process.resultSealed = true
		process.mu.Unlock()
		released()
		close(process.done)
	}()
	return process
}

func cloneRequest(request *protocol.Request) *protocol.Request {
	if request == nil {
		return nil
	}
	clone := *request
	clone.Command.Arguments = make([]string, len(request.Command.Arguments))
	copy(clone.Command.Arguments, request.Command.Arguments)
	clone.Command.Environment = make([]protocol.EnvironmentEntry, len(request.Command.Environment))
	copy(clone.Command.Environment, request.Command.Environment)
	if request.Command.Stdin != nil {
		stdin := *request.Command.Stdin
		clone.Command.Stdin = &stdin
	}
	return &clone
}

func (process *Process) Events() *EventReader { return process.events }

func (process *Process) Stdout() OutputSnapshot { return process.output.stdout.snapshot() }

func (process *Process) Stderr() OutputSnapshot { return process.output.stderr.snapshot() }

func (process *Process) Wait(ctx context.Context) (protocol.Settlement, error) {
	// A target's nonzero exit is product evidence, not a transport error. Callers
	// that require command success must use RequireSuccess after Wait.
	select {
	case <-process.done:
		return process.result()
	default:
	}
	select {
	case <-process.done:
		return process.result()
	case <-ctx.Done():
		// Terminal evidence wins when cancellation and settlement become visible
		// together, preventing a completed process from being reported as a wait
		// timeout solely because select chose the context branch.
		select {
		case <-process.done:
			return process.result()
		default:
		}
		reason := protocol.ControlReasonStop
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = protocol.ControlReasonDeadline
		}
		stopErr := process.requestStop(reason)
		select {
		case <-process.done:
			return process.result()
		default:
		}
		return protocol.Settlement{}, errors.Join(ctx.Err(), stopErr)
	}
}

func (process *Process) result() (protocol.Settlement, error) {
	process.mu.Lock()
	defer process.mu.Unlock()
	return cloneSettlement(process.settlement), process.resultErr
}

func (process *Process) joinEventReader() error {
	wait := func(limit time.Duration) bool {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-process.events.Done():
			return true
		case <-timer.C:
			return false
		}
	}
	if wait(eventReaderSettlementWait) {
		return eventReaderTerminalError(process.events)
	}
	timeoutErr := errors.New("test event reader did not settle after process-tree settlement")
	closeErr := process.events.closeSource()
	if !wait(eventReaderForcedCloseWait) {
		return errors.Join(timeoutErr, closeErr, errors.New("test event reader did not stop after its source was closed"))
	}
	return errors.Join(timeoutErr, closeErr, eventReaderTerminalError(process.events))
}

func eventReaderTerminalError(reader *EventReader) error {
	var closeErr error
	if err := reader.closeSource(); err != nil {
		closeErr = fmt.Errorf("close test event stream: %w", err)
	}
	if err := reader.Err(); err != nil {
		return errors.Join(fmt.Errorf("test event stream failed: %w", err), closeErr)
	}
	return closeErr
}

func cloneSettlement(settlement protocol.Settlement) protocol.Settlement {
	clone := settlement
	if settlement.Target.ExitCode != nil {
		exitCode := *settlement.Target.ExitCode
		clone.Target.ExitCode = &exitCode
	}
	if settlement.OwnerFailure != nil {
		ownerFailure := *settlement.OwnerFailure
		clone.OwnerFailure = &ownerFailure
	}
	if settlement.Platform.Root != nil {
		root := *settlement.Platform.Root
		if settlement.Platform.Root.ExitCode != nil {
			exitCode := *settlement.Platform.Root.ExitCode
			root.ExitCode = &exitCode
		}
		clone.Platform.Root = &root
	}
	if settlement.Platform.ActiveProcessCount != nil {
		active := *settlement.Platform.ActiveProcessCount
		clone.Platform.ActiveProcessCount = &active
	}
	return clone
}

func (process *Process) Stop(ctx context.Context) (protocol.Settlement, error) {
	select {
	case <-process.done:
		return process.Wait(ctx)
	default:
	}
	if err := process.requestStop(protocol.ControlReasonStop); err != nil {
		return protocol.Settlement{}, err
	}
	return process.Wait(ctx)
}

func (process *Process) stopWhenDone(ctx context.Context) {
	select {
	case <-process.done:
		return
	default:
	}
	select {
	case <-ctx.Done():
		// Cancellation can become ready at the same instant as natural settlement;
		// a second terminal check prevents publishing control into a retired session.
		select {
		case <-process.done:
			return
		default:
		}
		reason := protocol.ControlReasonStop
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = protocol.ControlReasonDeadline
		}
		_ = process.requestStop(reason)
	case <-process.done:
	}
}

func (process *Process) requestStop(reason string) error {
	for {
		process.mu.Lock()
		if process.terminal || process.stopSent {
			process.mu.Unlock()
			return nil
		}
		if process.stopActive {
			attempt := process.stopDone
			process.mu.Unlock()
			timer := time.NewTimer(stopAttemptJoinWait)
			select {
			case <-attempt:
				timer.Stop()
			case <-timer.C:
				return errors.New("concurrent process-owner stop publication did not join")
			}
			continue
		}
		process.stopActive = true
		process.stopDone = make(chan struct{})
		attempt := process.stopDone
		process.mu.Unlock()

		control := protocol.Control{
			SchemaVersion: protocol.ControlSchemaVersion,
			Identity:      process.identity,
			Reason:        reason,
		}
		err := process.session.stop(control)

		process.mu.Lock()
		resultErr := err
		if process.resultSealed {
			// A stop that outlives the bounded terminal join is no longer allowed to
			// rewrite the cached Wait result. The sealed result already records the
			// join failure; the initiating Stop caller still receives this late error.
		} else if err == nil {
			process.stopSent = true
			process.stopErr = nil
		} else if process.terminal && process.settlement.TerminationReason == protocol.TerminationNatural {
			// A naturally terminal process makes a concurrent stop unnecessary; a
			// transport error from that losing stop is not a lifecycle failure.
			process.stopErr = nil
			resultErr = nil
		} else {
			process.stopErr = err
			if !retryableControlPublication(err) {
				process.stopSent = true
			}
		}
		process.stopActive = false
		process.stopDone = nil
		close(attempt)
		process.mu.Unlock()
		return resultErr
	}
}

func RequireTreeEmpty(settlement protocol.Settlement) error {
	if err := protocol.ValidateSettlement(settlement); err != nil {
		return fmt.Errorf("validate process-owner settlement: %w", err)
	}
	if settlement.TreeState != protocol.TreeProvenEmpty || settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		return fmt.Errorf(
			"process tree cleanup failed: code=%s message=%s",
			settlement.Cleanup.FailureCode,
			settlement.Cleanup.FailureMessage,
		)
	}
	return nil
}

// RequireTreeEmptyForRequest applies the request-bound protocol oracle before
// evaluating cleanup. This is the consumer-side boundary for callers that
// retain the original request rather than trusting settlement input evidence.
func RequireTreeEmptyForRequest(settlement protocol.Settlement, request protocol.Request) error {
	if err := protocol.ValidateSettlementForRequest(settlement, request); err != nil {
		return fmt.Errorf("validate process-owner settlement for request: %w", err)
	}
	if settlement.TreeState != protocol.TreeProvenEmpty || settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		return fmt.Errorf(
			"process tree cleanup failed: code=%s message=%s",
			settlement.Cleanup.FailureCode,
			settlement.Cleanup.FailureMessage,
		)
	}
	return nil
}

func RequireSuccess(settlement protocol.Settlement, lifecycleErr error) error {
	if lifecycleErr != nil {
		return fmt.Errorf("owned process lifecycle failed: %w", lifecycleErr)
	}
	if err := RequireTreeEmpty(settlement); err != nil {
		return err
	}
	if settlement.TerminationReason != protocol.TerminationNatural || settlement.OwnerFailure != nil ||
		settlement.Target.Outcome != protocol.TargetExited ||
		settlement.Target.ExitCode == nil || *settlement.Target.ExitCode != 0 {
		return fmt.Errorf("owned target did not exit successfully: outcome=%s exit_code=%v", settlement.Target.Outcome, settlement.Target.ExitCode)
	}
	if settlement.Input.Outcome != protocol.InputNotRequested && settlement.Input.Outcome != protocol.InputDelivered {
		return fmt.Errorf("owned process input was not delivered: outcome=%s", settlement.Input.Outcome)
	}
	return nil
}
