package receive

import (
	"errors"
	"fmt"
	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/engine/recovery"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session"
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
	LocalSourceOmitted
)

// Failure retains the authoritative reason selected by application precedence.
// Clients translate these domain values to vocabulary without re-deciding it.
// When Fault is a terminal SessionTransport fault caused by failed session
// replacement, ReplacementCause carries that recovery-owned failure so
// transport-neutral transfer faults do not need connectivity vocabulary.
type Failure struct {
	Fault            transferfault.Fault
	Interruption     transfer.TransferInterruption
	Local            LocalFailure
	Code             FailureCode
	ReplacementCause error
	Cause            error
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

func (failure Failure) Unwrap() []error {
	switch {
	case failure.Cause != nil && failure.ReplacementCause != nil:
		return []error{failure.Cause, failure.ReplacementCause}
	case failure.Cause != nil:
		return []error{failure.Cause}
	case failure.ReplacementCause != nil:
		return []error{failure.ReplacementCause}
	default:
		return nil
	}
}

type Result struct {
	OutputAttention     recovery.AttentionReason
	Recovery            RecoveryOutcome
	Transfer            transfer.JobResult
	Operation           receivecontract.OperationID
	Job                 transfer.TransferJobID
	Destination         string
	DestinationAdjusted bool
	NameAdjustments     output.NameAdjustmentSummary
	Elapsed             time.Duration
	Connectivity        downloadmetrics.Snapshot
}
type SettlementInput struct {
	Result                                                                                      transfer.JobResult
	AdmissionError, RuntimeError, ConnectionError, ReplacementError, ContextError, CleanupError error
	Elapsed                                                                                     time.Duration
	Destination                                                                                 string
	DestinationAdjusted                                                                         bool
	NameAdjustments                                                                             output.NameAdjustmentSummary
	Operation                                                                                   receivecontract.OperationID
	Job                                                                                         transfer.TransferJobID
	Connectivity                                                                                downloadmetrics.Snapshot
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
	stop := shareStopSource(cause, failure.ReplacementCause)
	if stop != ShareStopUnconfirmed && failure.Code == 0 && class != task.FailureUsage {
		failure = Failure{Code: FailureShareStopped, Cause: cause}
		class = task.FailureNetwork
	}
	if cleanupErr != nil {
		failure = Failure{Cause: cleanupErr}
		class = task.FailureLocal
	}
	value := Result{Recovery: RecoveryOutcome{Reason: recoveryReason(class, failure, transfer.JobResult{}, cleanupErr), ShareStop: stop}}
	if attention, ok := errors.AsType[*output.AttentionError](cause); ok {
		value.OutputAttention = attention.Reason
	}
	return task.Completion[Result]{Value: value,
		Outcome: task.OutcomeFailed, FailureClass: class,
		Err: errors.Join(failure, cause), CleanupError: cleanupErr}
}

func Settle(input SettlementInput) (task.Completion[Result], error) {
	r := input.Result
	if r.TerminationInterruption != 0 && !r.TerminationInterruption.Valid() ||
		r.SettlementInterruption != 0 && !r.SettlementInterruption.Valid() ||
		!input.NameAdjustments.Valid() {
		return task.Completion[Result]{}, ErrInvalidResult
	}
	for _, file := range r.Files {
		if file.Interruption != 0 && !file.Interruption.Valid() {
			return task.Completion[Result]{}, ErrInvalidResult
		}
	}
	outcome := task.OutcomeFailed
	switch r.Outcome {
	case transfer.DirectTreeOutcomeSuccess:
		if successfulGetResult(r) && input.CleanupError == nil {
			outcome = task.OutcomeSuccess
			if r.Progress.SourceOmittedEntries != 0 {
				// Output publication can finish even when the source selection is incomplete.
				// Preserve its successful settlement so no recovery state is retained.
				outcome = task.OutcomePartial
			}
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
	case resultHasLocalStorageFault(r):
		class = task.FailureLocal
	case input.CleanupError != nil:
		class = task.FailureLocal
	case r.TerminationInterruption.Valid() || r.SettlementInterruption.Valid():
		class = task.FailureLocal
	case (r.Outcome == transfer.DirectTreeOutcomePaused || r.Outcome == transfer.DirectTreeOutcomeFailed) &&
		(input.AdmissionError != nil || input.RuntimeError != nil || input.ConnectionError != nil || input.ReplacementError != nil):
		class = task.FailureNetwork
	}
	result := task.Completion[Result]{
		Outcome: outcome, FailureClass: class, CleanupError: input.CleanupError,
		Value: Result{Transfer: r, Operation: input.Operation, Job: input.Job, Destination: input.Destination, DestinationAdjusted: input.DestinationAdjusted, NameAdjustments: input.NameAdjustments.Clone(), Elapsed: input.Elapsed, Connectivity: input.Connectivity},
	}
	if outcome != task.OutcomeSuccess {
		// The typed reason keeps application precedence while the error tree retains
		// diagnostic causes that may not have decided the user-visible outcome.
		stop := shareStopSource(input.RuntimeError, input.ConnectionError, input.ReplacementError, r.TerminationCause, r.SettlementFailure, r.SelectionResolutionFailure)
		failure := resultFailure(input, class)
		if failure.Local == LocalSourceOmitted {
			stop = ShareStopUnconfirmed
		}
		if stop != ShareStopUnconfirmed {
			failure, class = stoppedShareFailure(input, failure, class)
			result.FailureClass = class
			if outcome == task.OutcomePaused {
				result.Outcome = task.OutcomeFailed
			}
		}
		result.Value.Recovery = RecoveryOutcome{
			Reason:         recoveryReason(class, failure, r, input.CleanupError),
			ShareStop:      stop,
			HasSourceDrift: r.SourceDriftFault.Valid(),
		}
		result.Err = errors.Join(failure, input.diagnosticCauses())
	}
	return result, nil
}

func (input SettlementInput) diagnosticCauses() error {
	r := input.Result
	return errors.Join(input.AdmissionError, input.RuntimeError, input.ConnectionError,
		input.ReplacementError, input.ContextError, input.CleanupError, r.TerminationCause, r.SettlementFailure,
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
	return isTerminalSessionFault(r.TerminationFault)
}

func resultHasLocalStorageFault(r transfer.JobResult) bool {
	if isLocalStorageFault(r.TerminationFault) {
		return true
	}
	return !r.TerminationFault.Valid() && isLocalStorageFault(r.SettlementFault)
}

func isLocalStorageFault(value transferfault.Fault) bool {
	return value.Domain() == transferfault.DomainOutput || value.Domain() == transferfault.DomainCheckpoint
}

func isTerminalSessionFault(value transferfault.Fault) bool {
	return value.Domain() == transferfault.DomainSession && value.Scope() == transferfault.ScopeSessionTerminal
}

func IsTerminalSessionTransportFault(value transferfault.Fault) bool {
	code, ok := value.SessionCode()
	return ok && value.Scope() == transferfault.ScopeSessionTerminal && code == transferfault.SessionTransport
}

func shareStopSource(causes ...error) ShareStopSource {
	source := ShareStopUnconfirmed
	for _, cause := range causes {
		if session.PeerStopped(cause) {
			return ShareStopSender
		}
		if errors.Is(cause, relayset.ErrReceiverShareStopped) {
			source = ShareStopRelays
		} else if source == ShareStopUnconfirmed && errors.Is(cause, output.ErrShareStopped) {
			source = ShareStopRecorded
		}
	}
	return source
}

// A stop explains a network failure. Independent usage, source and output
// faults still tell the user why this particular attempt could not finish.
func stoppedShareFailure(input SettlementInput, primary Failure, class task.FailureClass) (Failure, task.FailureClass) {
	r := input.Result
	if class == task.FailureUsage || r.SourceDriftFault.Valid() || input.CleanupError != nil {
		return primary, class
	}
	if isLocalStorageFault(r.TerminationFault) {
		return Failure{Fault: r.TerminationFault}, task.FailureLocal
	}
	if code, ok := r.SettlementFault.OutputCode(); ok && code == transferfault.OutputContract {
		return Failure{Fault: r.SettlementFault}, task.FailureLocal
	}
	if class == task.FailureNetwork || r.TerminationFault.Domain() == transferfault.DomainSession {
		return Failure{Code: FailureShareStopped}, task.FailureNetwork
	}
	return primary, class
}

func resultFailure(input SettlementInput, class task.FailureClass) Failure {
	r := input.Result
	if class == task.FailureUsage {
		return Failure{Local: LocalSelectionMissing}
	}
	if r.SourceDriftFault.Valid() {
		return Failure{Fault: r.SourceDriftFault}
	}
	if r.TerminationFault.Valid() {
		failure := Failure{Fault: r.TerminationFault}
		if IsTerminalSessionTransportFault(r.TerminationFault) {
			failure.ReplacementCause = input.ReplacementError
		}
		return failure
	}
	if r.SettlementFault.Valid() {
		return Failure{Fault: r.SettlementFault}
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
		for _, f := range []transferfault.Fault{file.Fault, file.SettlementFault} {
			if f.Valid() {
				return Failure{Fault: f}
			}
		}
	}
	if r.Outcome == transfer.DirectTreeOutcomeSuccess && successfulGetResult(r) && r.Progress.SourceOmittedEntries != 0 {
		return Failure{Local: LocalSourceOmitted}
	}
	for _, cause := range []error{input.AdmissionError, input.RuntimeError, input.ConnectionError, input.ReplacementError, input.ContextError, r.TerminationCause, r.SettlementFailure, r.SelectionResolutionFailure, r.SourceDriftFailure} {
		if cause != nil {
			return Failure{Cause: cause}
		}
	}
	return Failure{}
}
