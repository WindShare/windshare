package commandprojection

import (
	"github.com/windshare/windshare/engine"
	"math"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
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
	spec := clievent.TransferResultSpec{Status: status, ExitCode: exit, Drift: drift, Elapsed: input.Value.Elapsed, Destination: clievent.NewDisplayPath(input.Value.Destination), DestinationAdjusted: input.Value.DestinationAdjusted,
		Files: projectFileOutcomes(r.Progress.FileOutcomes), DirectoryFailures: saturatingCount(len(r.Directories), r.OmittedDirectoryFailures), OmittedDiagnostics: saturatingAdd(r.OmittedDirectoryFailures, r.OmittedFileFailures), PublishedBytes: r.Progress.PublishedBytes, CountersExact: r.Progress.CountersExact}
	if input.Outcome != engine.OutcomeSuccess {
		spec.Failure, _ = ClassifyError(input.Err)
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

// The workflow's typed reason owns precedence over incidental diagnostic causes.
func projectReceiveFailure(value engine.ReceiveFailure) (clievent.Failure, bool) {
	if failure, ok := ProjectFault(value.Fault); ok {
		return failure, true
	}
	if failure, ok := ProjectTransferInterruption(value.Interruption); ok {
		return failure, true
	}
	local := map[engine.ReceiveLocalFailure]clievent.FailureCode{
		engine.ReceiveLocalSelectionMissing: clievent.FailureSelectionMissing, engine.ReceiveLocalRevisionConflict: clievent.FailureCheckpointRevisionConflict,
		engine.ReceiveLocalCheckpointInvalid: clievent.FailureCheckpointInvalid, engine.ReceiveLocalOwnedObjectUnknown: clievent.FailureOwnedObjectUnknown, engine.ReceiveLocalDestinationCollision: clievent.FailureDestinationCollision}
	if code, ok := local[value.Local]; ok {
		return mustFailure(code), true
	}
	if code, ok := ReceiveFailureCode(value.Code); ok {
		return mustFailure(code), true
	}
	return clievent.Failure{}, false
}
func ReceiveFailureCode(code engine.ReceiveFailureCode) (clievent.FailureCode, bool) {
	switch code {
	case engine.ReceiveFailureInvalidInput:
		return clievent.FailureInvalidInput, true
	case engine.ReceiveFailureOutputContract:
		return clievent.FailureOutputContract, true
	case engine.ReceiveFailureOutputFileAlreadyActive:
		return clievent.FailureOutputFileAlreadyActive, true
	case engine.ReceiveFailureOutputNeedsAttention:
		return clievent.FailureOutputNeedsAttention, true
	case engine.ReceiveFailureOutputOwnership:
		return clievent.FailureOutputOwnership, true
	case engine.ReceiveFailureOutputRecoveryUnavailable:
		return clievent.FailureOutputRecoveryUnavailable, true
	case engine.ReceiveFailurePeerConfiguration:
		return clievent.FailurePeerConfiguration, true
	case engine.ReceiveFailurePeerNegotiation:
		return clievent.FailurePeerNegotiation, true
	case engine.ReceiveFailurePeerProtocol:
		return clievent.FailurePeerProtocol, true
	case engine.ReceiveFailurePeerSignaling:
		return clievent.FailurePeerSignaling, true
	case engine.ReceiveFailurePeerStopped:
		return clievent.FailurePeerStopped, true
	default:
		return 0, false
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
	failure, _ := ClassifyError(input.Err)
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
