// Package processrun executes performance-evidence commands through the shared
// external process-owner backends without depending on correctness-test fixtures.
package processrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const (
	MaximumCapturedOutputBytes = 32 << 20
	DefaultCommandDeadline     = time.Hour
	DefaultTerminationGrace    = 10 * time.Second
)

var ErrOutputCaptureLimit = errors.New("owned command output capture limit exceeded")

type Spec struct {
	Identity         protocol.Identity
	Executable       string
	Arguments        []string
	WorkingDirectory string
	Environment      []protocol.EnvironmentEntry
	Deadline         time.Duration
	TerminationGrace time.Duration
	AuthorizeStart   func(protocol.StartEvidence) error
}

type Result struct {
	Stdout     []byte
	Stderr     []byte
	ProcessID  int
	ExitCode   int
	Settlement protocol.Settlement
}

type ExitError struct {
	Code int
}

func (failure *ExitError) Error() string {
	return fmt.Sprintf("owned command exited with code %d", failure.Code)
}

type Runner struct {
	MaximumOutput   int
	ValidateCleanup func(protocol.Settlement, protocol.Request) error
}

type startGateResult struct {
	evidence *protocol.StartEvidence
	err      error
}

type sessionResult struct {
	settlement protocol.Settlement
	start      startGateResult
	err        error
}

type platformSession interface {
	wait() sessionResult
	stop(protocol.Control) error
	close() error
}

func (runner Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	result := Result{ExitCode: -1}
	if ctx == nil {
		return result, errors.New("owned command context is nil")
	}
	request, err := requestFromSpec(spec)
	if err != nil {
		return result, err
	}
	helper, err := exactHelperPath()
	if err != nil {
		return result, err
	}
	maximumOutput := runner.MaximumOutput
	if maximumOutput == 0 {
		maximumOutput = MaximumCapturedOutputBytes
	}
	if maximumOutput < 1 || maximumOutput > MaximumCapturedOutputBytes {
		return result, fmt.Errorf(
			"owned command output limit must be in [1, %d]",
			MaximumCapturedOutputBytes,
		)
	}
	output := newProcessOutput(maximumOutput)
	session, err := startPlatform(ctx, helper, spec, request, output)
	if err != nil {
		return result, err
	}
	completed := make(chan sessionResult, 1)
	go func() { completed <- session.wait() }()

	var triggerErr error
	var stopErr error
	contextDone := ctx.Done()
	overflow := output.overflow()
	var completedResult sessionResult
	for {
		select {
		case completedResult = <-completed:
			goto settled
		case <-contextDone:
			if triggerErr == nil {
				triggerErr = context.Cause(ctx)
				reason := protocol.ControlReasonStop
				if errors.Is(triggerErr, context.DeadlineExceeded) {
					reason = protocol.ControlReasonDeadline
				}
				stopErr = session.stop(protocol.Control{
					SchemaVersion: protocol.ControlSchemaVersion,
					Identity:      request.Identity,
					Reason:        reason,
				})
			}
			contextDone = nil
		case <-overflow:
			if triggerErr == nil {
				triggerErr = ErrOutputCaptureLimit
				stopErr = session.stop(protocol.Control{
					SchemaVersion: protocol.ControlSchemaVersion,
					Identity:      request.Identity,
					Reason:        protocol.ControlReasonStop,
				})
			}
			overflow = nil
		}
	}

settled:
	closeErr := session.close()
	result.Stdout, result.Stderr = output.snapshot()
	var overflowErr error
	if output.exceeded() {
		overflowErr = ErrOutputCaptureLimit
	}
	result.Settlement = completedResult.settlement
	if completedResult.start.evidence != nil {
		result.ProcessID = completedResult.start.evidence.ProcessID
	}
	if completedResult.settlement.Target.ExitCode != nil {
		result.ExitCode = int(*completedResult.settlement.Target.ExitCode)
	}
	bindingErr := protocol.ValidateSettlementForRequest(completedResult.settlement, request)
	var cleanupErr error
	if bindingErr == nil {
		cleanupErr = requireCleanup(completedResult.settlement, request)
		if runner.ValidateCleanup != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				runner.ValidateCleanup(completedResult.settlement, request),
			)
		}
	}
	lifecycleErr := errors.Join(
		completedResult.err,
		stopErr,
		triggerErr,
		overflowErr,
		closeErr,
		bindingErr,
		cleanupErr,
	)
	if lifecycleErr != nil {
		return result, lifecycleErr
	}
	if completedResult.settlement.TerminationReason != protocol.TerminationNatural ||
		completedResult.settlement.OwnerFailure != nil {
		return result, fmt.Errorf(
			"owned command did not terminate naturally: reason=%s outcome=%s",
			completedResult.settlement.TerminationReason,
			completedResult.settlement.Target.Outcome,
		)
	}
	if result.ExitCode != 0 {
		return result, &ExitError{Code: result.ExitCode}
	}
	return result, nil
}

func requestFromSpec(spec Spec) (protocol.Request, error) {
	deadline, err := durationMilliseconds("deadline", spec.Deadline)
	if err != nil {
		return protocol.Request{}, err
	}
	grace, err := durationMilliseconds("termination grace", spec.TerminationGrace)
	if err != nil {
		return protocol.Request{}, err
	}
	request := protocol.NewRequest(spec.Identity, protocol.Command{
		Executable:       spec.Executable,
		Arguments:        append([]string{}, spec.Arguments...),
		WorkingDirectory: spec.WorkingDirectory,
		Environment:      append([]protocol.EnvironmentEntry(nil), spec.Environment...),
		Stdin:            nil,
	}, deadline, grace)
	if spec.AuthorizeStart == nil {
		return protocol.Request{}, errors.New("owned command requires a start authorizer")
	}
	if err := protocol.ValidateRequest(request); err != nil {
		return protocol.Request{}, fmt.Errorf("validate owned command: %w", err)
	}
	return request, nil
}

func durationMilliseconds(label string, duration time.Duration) (int64, error) {
	if duration <= 0 || duration%time.Millisecond != 0 {
		return 0, fmt.Errorf("%s must be a positive whole number of milliseconds", label)
	}
	return duration.Milliseconds(), nil
}

func requireCleanup(settlement protocol.Settlement, request protocol.Request) error {
	if err := protocol.ValidateSettlementForRequest(settlement, request); err != nil {
		return err
	}
	if settlement.TreeState != protocol.TreeProvenEmpty ||
		settlement.Cleanup.Outcome != protocol.CleanupCompleted {
		return fmt.Errorf(
			"owned command cleanup failed: tree=%s outcome=%s code=%s message=%s",
			settlement.TreeState,
			settlement.Cleanup.Outcome,
			settlement.Cleanup.FailureCode,
			settlement.Cleanup.FailureMessage,
		)
	}
	return nil
}
