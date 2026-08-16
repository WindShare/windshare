package commandprojection

import (
	"math"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type GetResultInput struct {
	Result              transfer.JobResult
	AdmissionError      error
	RuntimeError        error
	ConnectionError     error
	ContextError        error
	Elapsed             time.Duration
	Destination         clievent.DisplayPath
	DestinationAdjusted bool
}

func ProjectGetResult(input GetResultInput) (clievent.TransferResult, error) {
	status := projectResultStatus(input.Result)
	drift := clievent.DriftNone
	exit := clievent.ExitFailure
	switch {
	case input.Result.SourceDriftFault.Valid():
		drift, exit = clievent.DriftSource, clievent.ExitDrift
	case status == clievent.ResultSuccess:
		exit = clievent.ExitSuccess
	case provesMissingSelection(input.Result):
		exit = clievent.ExitUsage
	case (input.Result.Outcome == transfer.DirectTreeOutcomePaused ||
		input.Result.Outcome == transfer.DirectTreeOutcomeFailed) &&
		getResultHasNetworkAuthority(input):
		exit = clievent.ExitNetwork
	}
	failure, hasFailure := resultFailure(input, status, exit)
	spec := clievent.TransferResultSpec{
		Status: status, ExitCode: exit, Drift: drift, Elapsed: input.Elapsed,
		Destination: input.Destination, DestinationAdjusted: input.DestinationAdjusted,
		Files:             projectFileOutcomes(input.Result.Progress.FileOutcomes),
		DirectoryFailures: saturatingCount(len(input.Result.Directories), input.Result.OmittedDirectoryFailures),
		OmittedDiagnostics: saturatingAdd(
			input.Result.OmittedDirectoryFailures,
			input.Result.OmittedFileFailures,
		),
		PublishedBytes: input.Result.Progress.PublishedBytes,
		CountersExact:  input.Result.Progress.CountersExact,
	}
	if hasFailure {
		spec.Failure = failure
	}
	result, err := clievent.NewTransferResult(spec)
	if err != nil {
		return clievent.TransferResult{}, ErrInvalidProjection
	}
	return result, nil
}

func provesMissingSelection(result transfer.JobResult) bool {
	return result.Outcome == transfer.DirectTreeOutcomePartial &&
		result.Progress.Discovery == transfer.DiscoveryComplete &&
		result.Progress.CountersExact &&
		containsExactError(result.SelectionResolutionFailure, transfer.ErrSelectionTargetMissing)
}

func projectResultStatus(result transfer.JobResult) clievent.ResultStatus {
	switch result.Outcome {
	case transfer.DirectTreeOutcomeSuccess:
		if successfulGetResult(result) {
			return clievent.ResultSuccess
		}
		return clievent.ResultFailed
	case transfer.DirectTreeOutcomePartial:
		return clievent.ResultPartial
	case transfer.DirectTreeOutcomePaused:
		return clievent.ResultPaused
	case transfer.DirectTreeOutcomeFailed:
		return clievent.ResultFailed
	default:
		return clievent.ResultFailed
	}
}

func successfulGetResult(result transfer.JobResult) bool {
	progress := result.Progress
	files := progress.FileOutcomes
	return result.TerminationCause == nil && !result.TerminationFault.Valid() &&
		result.SettlementFailure == nil && !result.SettlementFault.Valid() &&
		result.SelectionResolutionFailure == nil && result.SourceDriftFailure == nil &&
		!result.SourceDriftFault.Valid() && len(result.Directories) == 0 && len(result.Files) == 0 &&
		result.OmittedDirectoryFailures == 0 && result.OmittedFileFailures == 0 &&
		files.PausedFiles == 0 && files.CollisionFiles == 0 && files.FailedFiles == 0 &&
		files.ItemBlockedFiles == 0 && result.Settlement.Kind() == transfer.DirectTreeSettlementSuccess &&
		progress.Discovery == transfer.DiscoveryComplete && progress.CountersExact &&
		progress.PublishedFiles == result.SucceededFiles &&
		progress.PublishedFiles == progress.DiscoveredFiles &&
		progress.PublishedBytes == progress.DiscoveredBytes &&
		progress.VerifiedBytes == progress.DiscoveredBytes
}

func getResultHasNetworkAuthority(input GetResultInput) bool {
	fault := input.Result.TerminationFault
	return input.AdmissionError != nil || input.RuntimeError != nil || input.ConnectionError != nil ||
		fault.Domain() == transferfault.DomainSession && fault.Scope() == transferfault.ScopeSessionTerminal
}

func resultFailure(
	input GetResultInput,
	status clievent.ResultStatus,
	exit clievent.ExitCode,
) (clievent.Failure, bool) {
	if status == clievent.ResultSuccess {
		return clievent.Failure{}, false
	}
	if exit == clievent.ExitUsage {
		return mustFailure(clievent.FailureSelectionMissing), true
	}
	for _, value := range []transferfault.Fault{
		input.Result.SourceDriftFault,
		input.Result.TerminationFault,
		input.Result.SettlementFault,
	} {
		if failure, ok := ProjectFault(value); ok {
			return failure, true
		}
	}
	for _, directory := range input.Result.Directories {
		if failure, ok := ProjectFault(directory.Fault); ok {
			return failure, true
		}
	}
	for _, file := range input.Result.Files {
		for _, value := range []transferfault.Fault{file.Fault, file.SettlementFault, file.LeaseReleaseFault} {
			if failure, ok := ProjectFault(value); ok {
				return failure, true
			}
		}
	}
	for _, cause := range []error{
		input.AdmissionError, input.RuntimeError, input.ConnectionError,
		input.ContextError, input.Result.TerminationCause,
		input.Result.SettlementFailure, input.Result.SelectionResolutionFailure,
		input.Result.SourceDriftFailure,
	} {
		if cause != nil {
			failure, _ := ClassifyError(cause)
			return failure, true
		}
	}
	return mustFailure(clievent.FailureUnexpected), true
}

type ShareFailureClass uint8

const (
	ShareFailureLocal ShareFailureClass = iota + 1
	ShareFailureNetwork
)

type ShareResultInput struct {
	Clean        bool
	Failure      error
	FailureClass ShareFailureClass
	Elapsed      time.Duration
}

func ProjectShareResult(input ShareResultInput) (clievent.ShareResult, error) {
	if input.Clean {
		if input.Failure != nil || input.FailureClass != 0 {
			return clievent.ShareResult{}, ErrInvalidProjection
		}
		result, err := clievent.NewShareResult(clievent.ShareResultSpec{
			ExitCode: clievent.ExitSuccess,
			Elapsed:  input.Elapsed,
		})
		if err != nil {
			return clievent.ShareResult{}, ErrInvalidProjection
		}
		return result, nil
	}
	if input.FailureClass != ShareFailureLocal && input.FailureClass != ShareFailureNetwork {
		return clievent.ShareResult{}, ErrInvalidProjection
	}
	failure, _ := ClassifyError(input.Failure)
	if input.Failure == nil {
		failure = mustFailure(clievent.FailureUnexpected)
	}
	exit := clievent.ExitFailure
	if input.FailureClass == ShareFailureNetwork {
		exit = clievent.ExitNetwork
	}
	result, err := clievent.NewShareResult(clievent.ShareResultSpec{
		ExitCode: exit, Elapsed: input.Elapsed, Failure: failure,
	})
	if err != nil {
		return clievent.ShareResult{}, ErrInvalidProjection
	}
	return result, nil
}

func ProjectCommandFailure(
	command clievent.Command,
	exit clievent.ExitCode,
	cause error,
) (clievent.CommandFailed, error) {
	failure, present := ClassifyError(cause)
	if !present {
		failure = mustFailure(clievent.FailureUnexpected)
	}
	event, err := clievent.NewCommandFailed(command, exit, failure)
	if err != nil {
		return clievent.CommandFailed{}, ErrInvalidProjection
	}
	return event, nil
}

func saturatingCount(retained int, omitted uint64) uint64 {
	if retained < 0 || uint64(retained) > math.MaxUint64-omitted {
		return math.MaxUint64
	}
	return uint64(retained) + omitted
}

func saturatingAdd(left, right uint64) uint64 {
	if right > math.MaxUint64-left {
		return math.MaxUint64
	}
	return left + right
}
