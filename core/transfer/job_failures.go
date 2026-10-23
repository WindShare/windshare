package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func normalizeCatalogBoundary(ctx context.Context, err error) error {
	return failurestate.Err(failurestate.Normalize(ctx, err, catalogBoundaryFault))
}

func normalizeSourceBoundary(ctx context.Context, err error) error {
	return failurestate.Err(failurestate.Normalize(ctx, err, sourceBoundaryFault))
}

func normalizeOutputBoundary(ctx context.Context, err error) error {
	return failurestate.Err(failurestate.Normalize(ctx, err, nil))
}

func catalogBoundaryFault(err error) (fault.Fault, bool) {
	switch {
	case errors.Is(err, catalog.ErrDirectoryStale):
		return mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale), true
	case failurestate.SessionTerminal(err):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), true
	default:
		return fault.Fault{}, false
	}
}

func sourceBoundaryFault(err error) (fault.Fault, bool) {
	if errors.Is(err, ErrBlockInvalidated) {
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), true
	}
	// Source evidence keeps its established precedence when transport cleanup
	// contributes another cause to the same immediate collaborator result.
	if value, known := failurestate.ContentFault(err); known {
		return value, true
	}
	switch {
	case errors.Is(err, ErrBlockIdentity):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), true
	case errors.Is(err, ErrBrokerClosed), errors.Is(err, transferlanes.ErrClosed), failurestate.SessionTerminal(err):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), true
	default:
		return fault.Fault{}, false
	}
}

func resourceBudgetFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSessionFault(fault.ScopeOutputPause, fault.SessionResourceBudget), cause,
	)
}

func sessionProtocolFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), cause,
	)
}

func sessionTransportFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), cause,
	)
}

func catalogDirectoryFailure(code fault.CatalogCode, cause error) *failurestate.Failure {
	return failurestate.New(mustCatalogFault(fault.ScopeDirectoryLocal, code), cause)
}

func catalogIntegrityFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration), cause,
	)
}

func sourceUnavailableFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceUnavailable), cause,
	)
}

func sourceChangedFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged), cause,
	)
}

func sourcePermanentFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSourceFault(fault.ScopeFileLocal, fault.SourcePermanent), cause,
	)
}

func sourceInvalidatedFailure(cause error) *failurestate.Failure {
	return failurestate.New(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), cause,
	)
}

func outputFailure(scope fault.Scope, code fault.OutputCode, cause error) *failurestate.Failure {
	value, err := fault.NewOutput(scope, code)
	if err != nil {
		return failurestate.DependencyContract(errors.Join(err, cause))
	}
	return failurestate.New(value, cause)
}

func outputContractFault(cause error) *failurestate.Failure {
	if cause == nil {
		cause = ErrOutputContract
	}
	return outputFailure(fault.ScopeOutputPause, fault.OutputContract, cause)
}

func requireOutputPause(err error) *failurestate.Failure {
	failure, ok := failurestate.Admit(err)
	if !ok {
		return failurestate.DependencyContract(err)
	}
	if failure.Canceled() || failure.Fault().Scope() >= fault.ScopeOutputPause {
		return failure
	}
	value := faultWithScope(failure.Fault(), fault.ScopeOutputPause)
	return failurestate.New(value, failure.Diagnostic())
}

func faultWithScope(value fault.Fault, scope fault.Scope) fault.Fault {
	switch value.Domain() {
	case fault.DomainSource:
		code, ok := value.SourceCode()
		if ok {
			return mustSourceFault(scope, code)
		}
	case fault.DomainCatalog:
		code, ok := value.CatalogCode()
		if ok {
			return mustCatalogFault(scope, code)
		}
	case fault.DomainSession:
		code, ok := value.SessionCode()
		if ok {
			return mustSessionFault(scope, code)
		}
	case fault.DomainOutput:
		code, ok := value.OutputCode()
		if ok {
			projected, _ := fault.NewOutput(scope, code)
			return projected
		}
	case fault.DomainCheckpoint:
		code, ok := value.CheckpointCode()
		if ok {
			projected, _ := fault.NewCheckpoint(scope, code)
			return projected
		}
	}
	return fault.DependencyContractFault()
}

func normalizedFault(err error) fault.Fault {
	return lifecyclePolicyFor(err).value
}

func closedLifecycleInterruption(failure *failurestate.Failure) TransferInterruption {
	if failure == nil || !failure.Canceled() {
		return 0
	}
	switch {
	case errors.Is(failure.Cancellation(), context.Canceled):
		return TransferInterruptionCanceled
	case errors.Is(failure.Cancellation(), context.DeadlineExceeded):
		return TransferInterruptionDeadline
	default:
		return 0
	}
}

func closedInterruption(err error) TransferInterruption {
	failure, ok := failurestate.Admit(err)
	if !ok {
		return 0
	}
	return closedLifecycleInterruption(failure)
}

func isSessionCode(value fault.Fault, expected fault.SessionCode) bool {
	code, ok := value.SessionCode()
	return ok && code == expected
}

func isJobTerminalError(err error) bool {
	return lifecyclePolicyFor(err).jobTerminal()
}

func mustSourceFault(scope fault.Scope, code fault.SourceCode) fault.Fault {
	value, _ := fault.NewSource(scope, code)
	return value
}

func mustCatalogFault(scope fault.Scope, code fault.CatalogCode) fault.Fault {
	value, _ := fault.NewCatalog(scope, code)
	return value
}

func mustSessionFault(scope fault.Scope, code fault.SessionCode) fault.Fault {
	value, _ := fault.NewSession(scope, code)
	return value
}

func mustOutputFault(scope fault.Scope, code fault.OutputCode) fault.Fault {
	value, _ := fault.NewOutput(scope, code)
	return value
}
