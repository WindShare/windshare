package outputsession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/fault"
)

var (
	ErrInvalidConfiguration         = errors.New("output session configuration is invalid")
	ErrSessionClosed                = errors.New("output session is closed")
	ErrSessionRequiresPause         = errors.New("output session requires pause")
	ErrDirectoryBinding             = errors.New("output directory claim conflicts with existing authority")
	ErrDirectoryChildrenUnsettled   = errors.New("output directory has unsettled child directories")
	ErrFileAlreadyActive            = errors.New("output file transaction is already active")
	ErrResourceBudget               = errors.New("output session resource budget is exhausted")
	ErrMutationAmbiguous            = errors.New("output mutation did not reach a provable stable cut")
	ErrExecutorContract             = errors.New("output executor violated its consumer contract")
	ErrConflictingSettlement        = errors.New("output session received a conflicting settlement request")
	ErrSessionResourceRelease       = errors.New("output session resources did not close cleanly")
	ErrTransactionOperationConflict = errors.New("output file transaction already settled through another operation")
)

func outputFault(scope fault.Scope, code fault.OutputCode, cause error) error {
	value, err := fault.NewOutput(scope, code)
	if err != nil {
		return fault.Wrap(fault.DependencyContractFault(), errors.Join(ErrExecutorContract, err))
	}
	return fault.Wrap(value, cause)
}

func resourceBudgetError() error {
	return outputFault(fault.ScopeOutputPause, fault.OutputResourceBudget, ErrResourceBudget)
}

func sessionClosedError() error {
	return outputFault(fault.ScopeOutputPause, fault.OutputOwnership, ErrSessionClosed)
}

func alreadyActiveError() error {
	return outputFault(fault.ScopeFileLocal, fault.OutputFileAlreadyActive, ErrFileAlreadyActive)
}

func executorContractError(cause error) error {
	return outputFault(fault.ScopeOutputPause, fault.OutputContract, errors.Join(ErrExecutorContract, cause))
}

func mutationAmbiguousError(cause error) error {
	return outputFault(fault.ScopeOutputPause, fault.OutputMutationAmbiguous, errors.Join(ErrMutationAmbiguous, cause))
}

func joinFailures(ctx context.Context, candidates ...error) error {
	return fault.ReduceBoundaryErrors(ctx, candidates...)
}

func joinCompletedFailures(candidates ...error) error {
	return fault.ReduceBoundaryErrorSet(candidates...)
}
