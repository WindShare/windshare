package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/fault"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func normalizeCatalogBoundary(ctx context.Context, err error) error {
	return fault.Normalize(ctx, err, catalogBoundaryFault).Err()
}

func normalizeSourceBoundary(ctx context.Context, err error) error {
	return fault.Normalize(ctx, err, sourceBoundaryFault).Err()
}

func normalizeOutputBoundary(ctx context.Context, err error) error {
	return fault.Normalize(ctx, err, nil).Err()
}

func catalogBoundaryFault(err error) (fault.Fault, bool) {
	switch {
	case errors.Is(err, catalog.ErrDirectoryStale):
		return mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale), true
	case fault.IsSessionTerminal(err):
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
	if value, known := fault.ClassifyContent(err); known {
		return value, true
	}
	switch {
	case errors.Is(err, ErrBlockIdentity):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), true
	case errors.Is(err, ErrBrokerClosed), errors.Is(err, transferlanes.ErrClosed), fault.IsSessionTerminal(err):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), true
	default:
		return fault.Fault{}, false
	}
}

func resourceBudgetFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSessionFault(fault.ScopeOutputPause, fault.SessionResourceBudget), cause)
}

func sessionProtocolFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), cause)
}

func sessionTransportFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), cause)
}

func catalogDirectoryFailure(code fault.CatalogCode, cause error) *fault.BoundaryError {
	return fault.New(mustCatalogFault(fault.ScopeDirectoryLocal, code), cause)
}

func catalogIntegrityFailure(cause error) *fault.BoundaryError {
	return fault.New(mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration), cause)
}

func sourceUnavailableFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSourceFault(fault.ScopeFileLocal, fault.SourceUnavailable), cause)
}

func sourceChangedFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged), cause)
}

func sourcePermanentFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSourceFault(fault.ScopeFileLocal, fault.SourcePermanent), cause)
}

func sourceInvalidatedFailure(cause error) *fault.BoundaryError {
	return fault.New(mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), cause)
}

func outputFailure(scope fault.Scope, code fault.OutputCode, cause error) *fault.BoundaryError {
	value, err := fault.NewOutput(scope, code)
	if err != nil {
		return fault.DependencyContract(errors.Join(err, cause))
	}
	return fault.New(value, cause)
}

func outputContractFault(cause error) *fault.BoundaryError {
	if cause == nil {
		cause = ErrOutputContract
	}
	return outputFailure(fault.ScopeOutputPause, fault.OutputContract, cause)
}

func requireOutputPause(err error) *fault.BoundaryError {
	failure, ok := fault.Admit(err)
	if !ok {
		return fault.DependencyContract(err)
	}
	if failure.Canceled() || failure.Fault().Scope() >= fault.ScopeOutputPause {
		return failure
	}
	value := faultWithScope(failure.Fault(), fault.ScopeOutputPause)
	return fault.New(value, failure.Cause())
}

func faultWithScope(value fault.Fault, scope fault.Scope) fault.Fault {
	switch value.Domain() {
	case fault.DomainSource:
		if code, ok := value.SourceCode(); ok {
			return mustSourceFault(scope, code)
		}
	case fault.DomainCatalog:
		if code, ok := value.CatalogCode(); ok {
			return mustCatalogFault(scope, code)
		}
	case fault.DomainSession:
		if code, ok := value.SessionCode(); ok {
			return mustSessionFault(scope, code)
		}
	case fault.DomainOutput:
		if code, ok := value.OutputCode(); ok {
			projected, _ := fault.NewOutput(scope, code)
			return projected
		}
	case fault.DomainCheckpoint:
		if code, ok := value.CheckpointCode(); ok {
			projected, _ := fault.NewCheckpoint(scope, code)
			return projected
		}
	}
	return fault.DependencyContractFault()
}

func normalizedFault(err error) fault.Fault {
	return lifecyclePolicyFor(err).value
}

func closedLifecycleInterruption(failure *fault.BoundaryError) TransferInterruption {
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
	failure, ok := fault.Admit(err)
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
