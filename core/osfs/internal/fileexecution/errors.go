package fileexecution

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/fault"
)

var (
	ErrInvalidConfiguration     = errors.New("file execution configuration is invalid")
	ErrInvalidClaim             = errors.New("file execution claim is invalid")
	ErrInvalidObservation       = errors.New("file execution observation is invalid")
	ErrCheckpointBinding        = errors.New("file checkpoint does not match the claimed file")
	ErrCheckpointNotInstalled   = errors.New("file checkpoint transition was not installed")
	ErrCheckpointObjectClaimed  = errors.New("file checkpoint object is already claimed by another lineage")
	ErrCheckpointRecordCapacity = errors.New("file checkpoint record capacity is exhausted")
	ErrPortContract             = errors.New("file execution collaborator contract is invalid")
	ErrObjectAllocation         = errors.New("file execution could not allocate an unclaimed object")
	ErrRangeOverlap             = errors.New("file range overlaps previously written or verified bytes")
	ErrRangeOutOfBounds         = errors.New("file range is outside the exact file size")
	ErrIncompleteFile           = errors.New("file publication requires complete verified ranges")
	ErrTransactionClosed        = errors.New("file transaction is closed")
	ErrRetirementUnauthorized   = errors.New("file retirement reason is not authorized")
	ErrRetirementAmbiguous      = errors.New("file retirement did not reach a provable ordered cut")
	ErrTargetOwnershipUnknown   = errors.New("file target ownership is unknown")
)

func newOutputFault(scope fault.Scope, code fault.OutputCode, cause error) error {
	value, err := fault.NewOutput(scope, code)
	if err != nil {
		return fault.Wrap(fault.DependencyContractFault(), errors.Join(fault.ErrInvalidFault, err, cause))
	}
	return fault.Wrap(value, cause)
}

func newCheckpointFault(scope fault.Scope, code fault.CheckpointCode, cause error) error {
	value, err := fault.NewCheckpoint(scope, code)
	if err != nil {
		return fault.Wrap(fault.DependencyContractFault(), errors.Join(fault.ErrInvalidFault, err, cause))
	}
	return fault.Wrap(value, cause)
}

func fileContractError(cause error) error {
	return newOutputFault(fault.ScopeFileLocal, fault.OutputContract, cause)
}

func fileStateError(cause error) error {
	return newOutputFault(fault.ScopeFileLocal, fault.OutputStateIO, cause)
}

func bindingError(cause error) error {
	return newOutputFault(fault.ScopeOutputPause, fault.OutputOwnership, cause)
}

func checkpointBindingError(cause error) error {
	return newCheckpointFault(fault.ScopeOutputPause, fault.CheckpointCorruptRecord, cause)
}

func checkpointInstallError(cause error) error {
	return newCheckpointFault(fault.ScopeOutputPause, fault.CheckpointStateIO, cause)
}

func collaboratorError(ctx context.Context, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	result := fault.NormalizeBoundary(ctx, cause)
	if _, ok := result.Fault(); ok {
		return cause
	}
	return fault.Wrap(fault.DependencyContractFault(), errors.Join(ErrPortContract, cause))
}

func joinFailures(ctx context.Context, candidates ...error) error {
	return fault.ReduceBoundaryErrors(ctx, candidates...)
}
