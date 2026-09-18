package receive

import (
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/task"
)

var ErrInvalidResult = errors.New("receive result violates its settlement contract")

type LocalFailure uint8

const (
	LocalFailureNone LocalFailure = iota
	LocalSelectionMissing
	LocalRevisionConflict
	LocalCheckpointInvalid
	LocalOwnedObjectUnknown
	LocalDestinationCollision
)

// Failure retains the authoritative reason selected by application precedence.
// Clients translate these domain values to vocabulary without re-deciding it.
type Failure struct {
	Fault        transferfault.Fault
	Interruption transfer.TransferInterruption
	Local        LocalFailure
	Code         FailureCode
	Cause        error
}

func (failure Failure) Error() string {
	switch {
	case failure.Fault.Valid():
		return fmt.Sprintf("receive fault: %v", failure.Fault)
	case failure.Interruption.Valid():
		return fmt.Sprintf("receive interrupted: %v", failure.Interruption)
	case failure.Local != LocalFailureNone:
		return fmt.Sprintf("receive local failure %d", failure.Local)
	case failure.Code != 0:
		return fmt.Sprintf("receive failure code %d", failure.Code)
	case failure.Cause != nil:
		return failure.Cause.Error()
	default:
		return "receive did not complete"
	}
}

func (failure Failure) Unwrap() error { return failure.Cause }

type Result struct {
	Transfer            transfer.JobResult
	Operation           receivecontract.OperationID
	Job                 transfer.TransferJobID
	Destination         string
	DestinationAdjusted bool
	Elapsed             time.Duration
	Connectivity        downloadmetrics.Snapshot
	ObservationLosses   []ObservationLoss
}
type SettlementInput struct {
	Result                                                                    transfer.JobResult
	AdmissionError, RuntimeError, ConnectionError, ContextError, CleanupError error
	Elapsed                                                                   time.Duration
	Destination                                                               string
	DestinationAdjusted                                                       bool
	Operation                                                                 receivecontract.OperationID
	Job                                                                       transfer.TransferJobID
	Connectivity                                                              downloadmetrics.Snapshot
}

func settlePreparationFailure(step stepOutcome, failure Failure, contextErr, cleanupErr error) task.Completion[Result] {
	class := task.FailureLocal
	switch step {
	case stepInvalidRequest:
		class = task.FailureUsage
	case stepNetworkFailure:
		class = task.FailureNetwork
	}
	if failure.Cause == nil && contextErr != nil {
		failure.Cause = contextErr
	}
	cause := failure.Cause
	if cleanupErr != nil {
		failure = Failure{Cause: cleanupErr}
		class = task.FailureLocal
	}
	return task.Completion[Result]{Settlement: task.Settlement{
		Outcome: task.OutcomeFailed, FailureClass: class,
		Err: errors.Join(failure, cause), CleanupError: cleanupErr,
	}}
}

func Settle(input SettlementInput) (task.Completion[Result], error) {
	r := input.Result
	if r.TerminationInterruption != 0 && !r.TerminationInterruption.Valid() || r.SettlementInterruption != 0 && !r.SettlementInterruption.Valid() {
		return task.Completion[Result]{}, ErrInvalidResult
	}
	outcome := task.OutcomeFailed
	switch r.Outcome {
	case transfer.DirectTreeOutcomeSuccess:
		if successfulGetResult(r) && input.CleanupError == nil {
			outcome = task.OutcomeSuccess
		}
	case transfer.DirectTreeOutcomePartial:
		outcome = task.OutcomePartial
	case transfer.DirectTreeOutcomePaused:
		outcome = task.OutcomePaused
	}
	if input.CleanupError != nil {
		outcome = task.OutcomeFailed
	}
	class := task.FailureLocal
	switch {
	case r.SourceDriftFault.Valid():
		class = task.FailureSourceDrift
	case outcome == task.OutcomeSuccess:
		class = task.FailureNone
	case provesMissingSelection(r):
		class = task.FailureUsage
	case resultHasTerminalNetworkFault(r):
		class = task.FailureNetwork
	case input.CleanupError != nil:
		class = task.FailureLocal
	case r.TerminationInterruption.Valid() || r.SettlementInterruption.Valid():
		class = task.FailureLocal
	case (r.Outcome == transfer.DirectTreeOutcomePaused || r.Outcome == transfer.DirectTreeOutcomeFailed) && (input.AdmissionError != nil || input.RuntimeError != nil || input.ConnectionError != nil):
		class = task.FailureNetwork
	}
	result := task.Completion[Result]{
		Settlement: task.Settlement{Outcome: outcome, FailureClass: class, CleanupError: input.CleanupError},
		Value:      Result{Transfer: r, Operation: input.Operation, Job: input.Job, Destination: input.Destination, DestinationAdjusted: input.DestinationAdjusted, Elapsed: input.Elapsed, Connectivity: input.Connectivity},
	}
	if outcome != task.OutcomeSuccess {
		// The typed reason keeps application precedence while the error tree retains
		// diagnostic causes that may not have decided the user-visible outcome.
		result.Err = errors.Join(resultFailure(input, class), input.diagnosticCauses())
	}
	return result, nil
}

func (input SettlementInput) diagnosticCauses() error {
	r := input.Result
	return errors.Join(input.AdmissionError, input.RuntimeError, input.ConnectionError,
		input.ContextError, input.CleanupError, r.TerminationCause, r.SettlementFailure,
		r.SelectionResolutionFailure, r.SourceDriftFailure)
}

func provesMissingSelection(r transfer.JobResult) bool {
	return r.Outcome == transfer.DirectTreeOutcomePartial && r.Progress.Discovery == transfer.DiscoveryComplete && r.Progress.CountersExact && containsExactError(r.SelectionResolutionFailure, transfer.ErrSelectionTargetMissing)
}
func successfulGetResult(r transfer.JobResult) bool {
	p := r.Progress
	f := p.FileOutcomes
	return r.TerminationCause == nil && !r.TerminationFault.Valid() && r.TerminationInterruption == 0 &&
		r.SettlementFailure == nil && !r.SettlementFault.Valid() && r.SettlementInterruption == 0 &&
		r.SelectionResolutionFailure == nil && r.SourceDriftFailure == nil && !r.SourceDriftFault.Valid() &&
		len(r.Directories) == 0 && len(r.Files) == 0 && r.OmittedDirectoryFailures == 0 && r.OmittedFileFailures == 0 &&
		f.PausedFiles == 0 && f.CollisionFiles == 0 && f.FailedFiles == 0 && f.ItemBlockedFiles == 0 &&
		r.Settlement.Kind() == transfer.DirectTreeSettlementSuccess && p.Discovery == transfer.DiscoveryComplete && p.CountersExact &&
		p.PublishedFiles == r.SucceededFiles && p.PublishedFiles == p.DiscoveredFiles && p.PublishedBytes == p.DiscoveredBytes &&
		p.PreviouslyPublishedBytes <= p.DiscoveredBytes && p.VerifiedBytes == p.DiscoveredBytes-p.PreviouslyPublishedBytes
}
func resultHasTerminalNetworkFault(r transfer.JobResult) bool {
	return r.TerminationFault.Domain() == transferfault.DomainSession && r.TerminationFault.Scope() == transferfault.ScopeSessionTerminal
}
func resultFailure(input SettlementInput, class task.FailureClass) Failure {
	r := input.Result
	if class == task.FailureUsage {
		return Failure{Local: LocalSelectionMissing}
	}
	for _, f := range []transferfault.Fault{r.SourceDriftFault, r.TerminationFault, r.SettlementFault} {
		if f.Valid() {
			return Failure{Fault: f}
		}
	}
	// A real cleanup error remains authoritative when interruption happened too.
	if input.CleanupError != nil {
		return Failure{Cause: input.CleanupError}
	}
	for _, v := range []transfer.TransferInterruption{r.TerminationInterruption, r.SettlementInterruption} {
		if v.Valid() {
			return Failure{Interruption: v}
		}
	}
	f := r.Progress.FileOutcomes
	for _, v := range []struct {
		count uint64
		kind  LocalFailure
	}{{f.RevisionConflictFiles, LocalRevisionConflict}, {f.CheckpointInvalidFiles, LocalCheckpointInvalid}, {f.OwnedObjectUnknownFiles, LocalOwnedObjectUnknown}, {f.CollisionFiles, LocalDestinationCollision}} {
		if v.count > 0 {
			return Failure{Local: v.kind}
		}
	}
	for _, d := range r.Directories {
		if d.Fault.Valid() {
			return Failure{Fault: d.Fault}
		}
	}
	for _, file := range r.Files {
		for _, f := range []transferfault.Fault{file.Fault, file.SettlementFault, file.LeaseReleaseFault} {
			if f.Valid() {
				return Failure{Fault: f}
			}
		}
	}
	for _, cause := range []error{input.AdmissionError, input.RuntimeError, input.ConnectionError, input.ContextError, r.TerminationCause, r.SettlementFailure, r.SelectionResolutionFailure, r.SourceDriftFailure} {
		if cause != nil {
			return Failure{Cause: cause}
		}
	}
	return Failure{}
}
