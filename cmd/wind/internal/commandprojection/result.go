package commandprojection

import (
	"math"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

// ProjectReceiveResult translates an already settled application outcome.
func ProjectReceiveResult(input engine.TaskCompletion[engine.ReceiveResult]) (clievent.TransferResult, error) {
	status := clievent.ResultFailed
	switch input.Outcome {
	case engine.OutcomeSuccess:
		status = clievent.ResultSuccess
	case engine.OutcomePartial:
		status = clievent.ResultPartial
	case engine.OutcomePaused:
		status = clievent.ResultPaused
	}
	exit := projectApplicationFailureClass(input.FailureClass)
	drift := clievent.DriftNone
	if input.FailureClass == engine.FailureSourceDrift {
		drift = clievent.DriftSource
	}
	r := input.Value.Transfer
	spec := clievent.TransferResultSpec{Status: status, ExitCode: exit, Drift: drift, Elapsed: input.Value.Elapsed, Destination: appobs.NewDisplayPath(input.Value.Destination), DestinationAdjusted: input.Value.DestinationAdjusted,
		Files: projectFileOutcomes(r.Progress.FileOutcomes), DirectoryFailures: saturatingCount(len(r.Directories), r.OmittedDirectoryFailures), OmittedDiagnostics: saturatingAdd(r.OmittedDirectoryFailures, r.OmittedFileFailures), PublishedBytes: r.Progress.PublishedBytes, CountersExact: r.Progress.CountersExact}
	if input.Outcome != engine.OutcomeSuccess {
		spec.Failure = input.Failure
	}
	value, err := clievent.NewTransferResult(spec)
	if err != nil {
		return clievent.TransferResult{}, ErrInvalidProjection
	}
	return value, nil
}
func projectApplicationFailureClass(class engine.FailureClass) clievent.ExitCode {
	switch class {
	case engine.FailureNone:
		return clievent.ExitSuccess
	case engine.FailureNetwork:
		return clievent.ExitNetwork
	case engine.FailureUsage:
		return clievent.ExitUsage
	case engine.FailureSourceDrift:
		return clievent.ExitDrift
	default:
		return clievent.ExitFailure
	}
}

func ProjectShareResult(input engine.TaskResult[engine.ShareResult]) (clievent.ShareResult, error) {
	exit := clievent.ExitSuccess
	switch input.FailureClass {
	case engine.FailureNone:
	case engine.FailureUsage:
		exit = clievent.ExitUsage
	case engine.FailureNetwork:
		exit = clievent.ExitNetwork
	case engine.FailureSourceDrift:
		exit = clievent.ExitDrift
	case engine.FailureLocal:
		exit = clievent.ExitFailure
	default:
		return clievent.ShareResult{}, ErrInvalidProjection
	}
	failure := input.Failure
	result, err := clievent.NewShareResult(clievent.ShareResultSpec{ExitCode: exit, Elapsed: input.Value.Elapsed, Failure: failure})
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
		failure = mustFailure(appobs.FailureUnexpected)
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
