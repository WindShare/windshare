package transfer

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

type cancellationKind uint8

const (
	cancellationNone cancellationKind = iota
	cancellationCanceled
	cancellationDeadline
)

// lifecycleFailure keeps the closed policy state distinct from its raw
// diagnostic. It deliberately has no Unwrap method, so code outside the
// immediate boundary cannot reinterpret the diagnostic graph as authority.
type lifecycleFailure struct {
	policy       lifecyclePolicy
	cancellation cancellationKind
	diagnostic   error
}

var lifecycleFailureType = reflect.TypeFor[*lifecycleFailure]()

func (failure *lifecycleFailure) Error() string {
	if failure == nil {
		return "transfer lifecycle failure is invalid"
	}
	switch {
	case failure.policy.canceled && failure.policy.value.Valid() && failure.diagnostic != nil:
		return fmt.Sprintf("transfer canceled with fault %s: %v", failure.policy.value, failure.diagnostic)
	case failure.policy.canceled && failure.diagnostic != nil:
		return fmt.Sprintf("transfer canceled: %v", failure.diagnostic)
	case failure.policy.canceled:
		return "transfer canceled"
	case failure.policy.value.Valid() && failure.diagnostic != nil:
		return fmt.Sprintf("transfer fault %s: %v", failure.policy.value, failure.diagnostic)
	case failure.policy.value.Valid():
		return fmt.Sprintf("transfer fault %s", failure.policy.value)
	default:
		return "transfer lifecycle failure is invalid"
	}
}

// Is exposes only the closed cancellation signal. Native diagnostic causes are
// intentionally not searchable after normalization.
func (failure *lifecycleFailure) Is(target error) bool {
	return failure != nil && failure.policy.canceled && failure.cancellation.matches(target)
}

// admitLifecycleFailure accepts only the direct concrete carrier produced by
// this package. The dynamic-type gate prevents errors.As from granting policy
// authority to an arbitrary wrapper while still using the standard admission
// API after that exactness proof.
func admitLifecycleFailure(err error) (*lifecycleFailure, bool) {
	if err == nil || reflect.TypeOf(err) != lifecycleFailureType {
		return nil, false
	}
	var failure *lifecycleFailure
	if !errors.As(err, &failure) || failure == nil {
		return nil, false
	}
	return failure, true
}

func admitInternalFailure(err error) *lifecycleFailure {
	if err == nil {
		return nil
	}
	if failure, ok := admitLifecycleFailure(err); ok {
		return failure
	}
	return dependencyContractFailure(err)
}

func lifecycleError(failure *lifecycleFailure) error {
	if failure == nil {
		return nil
	}
	return failure
}

func (kind cancellationKind) cause() error {
	switch kind {
	case cancellationCanceled:
		return context.Canceled
	case cancellationDeadline:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (kind cancellationKind) matches(target error) bool {
	return kind != cancellationNone && errors.Is(kind.cause(), target)
}

func joinCancellation(left, right cancellationKind) cancellationKind {
	if right > left {
		return right
	}
	return left
}

var (
	contextCanceledType = reflect.TypeOf(context.Canceled)
	contextDeadlineType = reflect.TypeOf(context.DeadlineExceeded)
)

// exactCancellation admits only the standard sentinel itself. Internal joins
// must not let an arbitrary wrapper graph acquire cancellation authority after
// the collaborator boundary has already normalized it.
func exactCancellation(err error) cancellationKind {
	dynamicType := reflect.TypeOf(err)
	switch {
	case dynamicType == contextDeadlineType && errors.Is(err, context.DeadlineExceeded):
		return cancellationDeadline
	case dynamicType == contextCanceledType && errors.Is(err, context.Canceled):
		return cancellationCanceled
	default:
		return cancellationNone
	}
}

func normalizeCatalogBoundary(ctx context.Context, err error) error {
	return lifecycleError(normalizeCollaboratorBoundary(ctx, err, catalogBoundaryFault))
}

func normalizeSourceBoundary(ctx context.Context, err error) error {
	return lifecycleError(normalizeCollaboratorBoundary(ctx, err, sourceBoundaryFault))
}

func normalizeOutputBoundary(ctx context.Context, err error) error {
	return lifecycleError(normalizeCollaboratorBoundary(ctx, err, nil))
}

type boundaryFaultClassifier func(error) (fault.Fault, bool)

// normalizeCollaboratorBoundary is the only place TransferJob uses errors.Is/As
// on a collaborator result. Policy code receives lifecycleFailure and therefore
// cannot recover authority from wrapping shape or joined error topology.
func normalizeCollaboratorBoundary(
	ctx context.Context,
	err error,
	classify boundaryFaultClassifier,
) *lifecycleFailure {
	if existing := closedContextCause(ctx); existing != nil {
		return existing
	}
	if cancellation := boundaryCancellation(ctx, err); cancellation != cancellationNone {
		diagnostic := err
		if diagnostic == nil && ctx != nil {
			diagnostic = context.Cause(ctx)
		}
		return &lifecycleFailure{
			policy: lifecyclePolicy{canceled: true}, cancellation: cancellation, diagnostic: diagnostic,
		}
	}
	if existing, ok := admitLifecycleFailure(err); ok {
		return existing
	}
	if err == nil {
		return nil
	}
	var normalized *fault.BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.Fault().Valid() {
		return newFaultFailure(normalized.Fault(), err)
	}
	if classify != nil {
		if value, known := classify(err); known {
			return newFaultFailure(value, err)
		}
	}
	return newFaultFailure(fault.DependencyContractFault(), err)
}

func boundaryCancellation(ctx context.Context, err error) cancellationKind {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return cancellationCanceled
		case context.DeadlineExceeded:
			return cancellationDeadline
		}
	}
	if err == nil {
		return cancellationNone
	}
	if errors.Is(err, context.Canceled) {
		return cancellationCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return cancellationDeadline
	}
	return cancellationNone
}

func catalogBoundaryFault(err error) (fault.Fault, bool) {
	switch {
	case errors.Is(err, catalog.ErrDirectoryStale):
		return mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale), true
	case isRawSessionTerminal(err):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), true
	default:
		return fault.Fault{}, false
	}
}

func sourceBoundaryFault(err error) (fault.Fault, bool) {
	switch {
	case errors.Is(err, content.ErrRevisionDrift), errors.Is(err, ErrBlockInvalidated):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), true
	case errors.Is(err, content.ErrRevisionStale), errors.Is(err, content.ErrSourceDrift):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged), true
	case errors.Is(err, content.ErrRevisionNotFound), errors.Is(err, content.ErrRevisionUnreadable),
		errors.Is(err, content.ErrUnsupportedStability):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourcePermanent), true
	case errors.Is(err, content.ErrQuotaExceeded), errors.Is(err, content.ErrLeaseExpired),
		errors.Is(err, content.ErrInvalidLease):
		return mustSourceFault(fault.ScopeFileLocal, fault.SourceUnavailable), true
	case errors.Is(err, ErrBlockIdentity):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), true
	case errors.Is(err, ErrBrokerClosed), errors.Is(err, ErrLaneClosed), isRawSessionTerminal(err):
		return mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), true
	default:
		return fault.Fault{}, false
	}
}

func isRawSessionTerminal(err error) bool {
	return errors.Is(err, protocolsession.ErrSessionTerminated) ||
		errors.Is(err, protocolsession.ErrPeerSessionTerminal) ||
		errors.Is(err, protocolsession.ErrWriterTerminal) ||
		errors.Is(err, protocolsession.ErrWriterStopped)
}

func newFaultFailure(value fault.Fault, diagnostic error) *lifecycleFailure {
	if !value.Valid() {
		value = fault.DependencyContractFault()
	}
	return &lifecycleFailure{
		policy:     lifecyclePolicy{value: value},
		diagnostic: diagnostic,
	}
}

func cancellationFailure(ctx context.Context, diagnostic error) *lifecycleFailure {
	if existing, ok := admitLifecycleFailure(diagnostic); ok {
		return existing
	}
	if existing := closedContextCause(ctx); existing != nil {
		return existing
	}
	cancellation := boundaryCancellation(ctx, diagnostic)
	if cancellation == cancellationNone {
		return dependencyContractFailure(diagnostic)
	}
	if diagnostic == nil && ctx != nil {
		diagnostic = context.Cause(ctx)
	}
	return &lifecycleFailure{
		policy: lifecyclePolicy{canceled: true}, cancellation: cancellation, diagnostic: diagnostic,
	}
}

func closedContextCause(ctx context.Context) *lifecycleFailure {
	if ctx == nil {
		return nil
	}
	failure, _ := admitLifecycleFailure(context.Cause(ctx))
	return failure
}

func dependencyContractFailure(cause error) *lifecycleFailure {
	return newFaultFailure(fault.DependencyContractFault(), cause)
}

func resourceBudgetFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSessionFault(fault.ScopeOutputPause, fault.SessionResourceBudget), cause,
	)
}

func sessionProtocolFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol), cause,
	)
}

func sessionTransportFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport), cause,
	)
}

func catalogDirectoryFailure(code fault.CatalogCode, cause error) *lifecycleFailure {
	return newFaultFailure(mustCatalogFault(fault.ScopeDirectoryLocal, code), cause)
}

func catalogIntegrityFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration), cause,
	)
}

func sourceUnavailableFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceUnavailable), cause,
	)
}

func sourceChangedFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged), cause,
	)
}

func sourcePermanentFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSourceFault(fault.ScopeFileLocal, fault.SourcePermanent), cause,
	)
}

func sourceInvalidatedFailure(cause error) *lifecycleFailure {
	return newFaultFailure(
		mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated), cause,
	)
}

func outputFailure(scope fault.Scope, code fault.OutputCode, cause error) *lifecycleFailure {
	value, err := fault.NewOutput(scope, code)
	if err != nil {
		return dependencyContractFailure(errors.Join(err, cause))
	}
	return newFaultFailure(value, cause)
}

func outputContractFault(cause error) *lifecycleFailure {
	if cause == nil {
		cause = ErrOutputContract
	}
	return outputFailure(fault.ScopeOutputPause, fault.OutputContract, cause)
}

func requireOutputPause(err error) *lifecycleFailure {
	failure, ok := admitLifecycleFailure(err)
	if !ok {
		return dependencyContractFailure(err)
	}
	if failure.policy.canceled || failure.policy.value.Scope() >= fault.ScopeOutputPause {
		return failure
	}
	value := faultWithScope(failure.policy.value, fault.ScopeOutputPause)
	return &lifecycleFailure{
		policy:     lifecyclePolicy{value: value},
		diagnostic: failure.diagnostic,
	}
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

func joinLifecycleFailures(candidates ...error) error {
	return lifecycleError(reduceLifecycleFailures(candidates...))
}

func joinClosedLifecycleFailures(candidates ...*lifecycleFailure) *lifecycleFailure {
	errorsToReduce := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil {
			errorsToReduce = append(errorsToReduce, candidate)
		}
	}
	return reduceLifecycleFailures(errorsToReduce...)
}

func mergeLifecycleFailures(
	current *lifecycleFailure,
	candidates ...error,
) *lifecycleFailure {
	reduced := reduceLifecycleFailures(candidates...)
	switch {
	case current == nil:
		return reduced
	case reduced == nil:
		return current
	default:
		return joinClosedLifecycleFailures(current, reduced)
	}
}

func reduceLifecycleFailures(candidates ...error) *lifecycleFailure {
	var joined fault.Fault
	var cancellation cancellationKind
	var soleDirect *lifecycleFailure
	candidateCount := 0
	diagnostics := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		candidateCount++
		failure, ok := admitLifecycleFailure(candidate)
		if candidateCount == 1 && ok {
			soleDirect = failure
		} else {
			soleDirect = nil
		}
		if !ok {
			if exact := exactCancellation(candidate); exact != cancellationNone {
				failure = &lifecycleFailure{
					policy: lifecyclePolicy{canceled: true}, cancellation: exact, diagnostic: candidate,
				}
			} else {
				failure = dependencyContractFailure(candidate)
			}
		}
		joined = fault.Join(joined, failure.policy.value)
		if failure.policy.canceled {
			cancellation = joinCancellation(cancellation, failure.cancellation)
		}
		if failure.diagnostic != nil {
			diagnostics = append(diagnostics, failure.diagnostic)
		}
	}
	if candidateCount == 1 && soleDirect != nil {
		// Keeping a singleton carrier intact preserves the internal provenance of a
		// cancel cause. A reduction is necessary only when more than one closed
		// policy value must actually be joined.
		return soleDirect
	}
	if !joined.Valid() && cancellation == cancellationNone {
		return nil
	}
	return &lifecycleFailure{
		policy:       lifecyclePolicy{value: joined, canceled: cancellation != cancellationNone},
		cancellation: cancellation,
		diagnostic:   errors.Join(diagnostics...),
	}
}

// collaboratorError projects an internal normalized reduction back across an
// error-returning port without exposing lifecycleFailure outside TransferJob.
func collaboratorError(failure *lifecycleFailure, diagnostic error) error {
	if failure == nil {
		return fault.Wrap(fault.DependencyContractFault(), diagnostic)
	}
	if failure.policy.canceled {
		if cancellation := failure.cancellation.cause(); cancellation != nil {
			return cancellation
		}
		return context.Canceled
	}
	return fault.Wrap(failure.policy.value, diagnostic)
}

func normalizedFault(err error) fault.Fault {
	return lifecyclePolicyFor(err).value
}

func closedFault(err error) fault.Fault {
	failure, ok := admitLifecycleFailure(err)
	if !ok {
		return fault.Fault{}
	}
	return failure.policy.value
}

func closedLifecycleFault(failure *lifecycleFailure) fault.Fault {
	if failure == nil {
		return fault.Fault{}
	}
	return failure.policy.value
}

func closedLifecycleInterruption(failure *lifecycleFailure) TransferInterruption {
	if failure == nil || !failure.policy.canceled {
		return 0
	}
	switch failure.cancellation {
	case cancellationCanceled:
		return TransferInterruptionCanceled
	case cancellationDeadline:
		return TransferInterruptionDeadline
	default:
		return 0
	}
}

func closedInterruption(err error) TransferInterruption {
	failure, ok := admitLifecycleFailure(err)
	if !ok {
		return 0
	}
	return closedLifecycleInterruption(failure)
}

func lifecycleDiagnostic(err error) error {
	failure, ok := admitLifecycleFailure(err)
	if !ok {
		return err
	}
	return failure.diagnostic
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
