package v2peer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

const maxReceiverErrorDepth = 64

var errReceiverOpaqueCause = errors.New("receiver operation retained an opaque failure")

// receiverFilteredError preserves outer phase context when a benign leaf is
// stripped from a wrapped error without making the stripped leaf reachable via
// errors.Is.
type receiverFilteredError struct {
	message string
	cause   error
}

func (failure *receiverFilteredError) Error() string { return failure.message }
func (failure *receiverFilteredError) Unwrap() error { return failure.cause }

type receiverCausePolicy struct {
	contextCanceled  bool
	operationMissing ReceiverBenignCause
}

type receiverCauseClassification struct {
	retained error
	benign   []ReceiverBenignCause
	classes  []ReceiverCauseClass
	complete bool
}

func classifyReceiverCause(cause error, policy receiverCausePolicy) receiverCauseClassification {
	if cause == nil {
		return receiverCauseClassification{complete: true}
	}
	classification := classifyReceiverCauseAtDepth(cause, policy, 0)
	classification.benign = uniqueReceiverBenignCauses(classification.benign)
	classification.classes = uniqueReceiverCauseClasses(classification.classes)
	return classification
}

func classifyReceiverCauseAtDepth(
	cause error,
	policy receiverCausePolicy,
	depth int,
) receiverCauseClassification {
	if cause == nil {
		return receiverCauseClassification{complete: true}
	}
	if depth >= maxReceiverErrorDepth {
		return receiverCauseClassification{
			retained: errReceiverOpaqueCause,
			classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
		}
	}
	// Comparing an arbitrary error against pointer sentinels (*errors.errorString)
	// cannot panic in Go even when cause has an uncomparable dynamic type.
	if policy.contextCanceled && cause == context.Canceled { //nolint:errorlint // Inspect one node at a time so joined siblings stay classified.
		return receiverCauseClassification{
			benign:   []ReceiverBenignCause{ReceiverBenignContextCanceled},
			complete: true,
		}
	}
	if policy.operationMissing != "" && cause == session.ErrOperationMissing { //nolint:errorlint // Inspect one node at a time so joined siblings stay classified.
		return receiverCauseClassification{
			benign:   []ReceiverBenignCause{policy.operationMissing},
			complete: true,
		}
	}
	if boundary, ok := cause.(*transferfault.BoundaryError); ok { //nolint:errorlint // Inspect the current error node before descending into Unwrap.
		if boundary == nil || !boundary.Fault().Valid() {
			return receiverCauseClassification{
				retained: errReceiverOpaqueCause,
				classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
				complete: true,
			}
		}
		child := boundary.Unwrap()
		var childClassified receiverCauseClassification
		if child == nil {
			childClassified = receiverCauseClassification{
				retained: errReceiverOpaqueCause,
				classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
			}
		} else {
			// The normalized fault is the lifecycle authority; keep its diagnostic
			// child intact without applying local benign filtering.
			childClassified = classifyReceiverCauseAtDepth(child, receiverCausePolicy{}, depth+1)
		}
		if boundary.Fault().Scope() == transferfault.ScopeSessionTerminal ||
			boundary.Fault().Domain() == transferfault.DomainSession {
			childClassified.classes = append([]ReceiverCauseClass{ReceiverCauseProtocol}, childClassified.classes...)
		}
		return childClassified
	}
	if class, known := directReceiverCauseClass(cause); known {
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{class},
			complete: true,
		}
	}
	if multi, ok := cause.(interface{ Unwrap() []error }); ok {
		return classifyReceiverMultiCause(multi.Unwrap(), policy, depth)
	}
	if single, ok := cause.(interface{ Unwrap() error }); ok {
		return classifyReceiverSingleCause(cause, single.Unwrap(), policy, depth)
	}
	if _, ok := cause.(receiversession.RemoteOperationError); ok { //nolint:errorlint // Leaf check after Unwrap traversal.
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{ReceiverCauseProtocol},
			complete: true,
		}
	}
	return receiverCauseClassification{
		retained: cause,
		classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
		complete: true,
	}
}

func classifyReceiverMultiCause(
	children []error,
	policy receiverCausePolicy,
	depth int,
) receiverCauseClassification {
	var retained []error
	var benign []ReceiverBenignCause
	var classes []ReceiverCauseClass
	complete := true
	for _, child := range children {
		if child == nil {
			continue
		}
		classified := classifyReceiverCauseAtDepth(child, policy, depth+1)
		if classified.retained != nil {
			retained = append(retained, classified.retained)
		}
		benign = append(benign, classified.benign...)
		classes = append(classes, classified.classes...)
		complete = complete && classified.complete
	}
	return receiverCauseClassification{
		retained: joinReceiverResiduals(retained),
		benign:   benign,
		classes:  classes,
		complete: complete,
	}
}

func classifyReceiverSingleCause(
	cause error,
	child error,
	policy receiverCausePolicy,
	depth int,
) receiverCauseClassification {
	if child == nil {
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
			complete: true,
		}
	}
	classified := classifyReceiverCauseAtDepth(child, policy, depth+1)
	if classified.retained == nil {
		return classified
	}
	if len(classified.benign) == 0 && classified.complete && !containsBoundaryError(child) {
		classified.retained = cause
		return classified
	}
	classified.retained = &receiverFilteredError{message: cause.Error(), cause: classified.retained}
	return classified
}

func containsBoundaryError(cause error) bool {
	_, ok := errors.AsType[*transferfault.BoundaryError](cause)
	return ok
}

func annotateReceiverDecisionDiagnostics(
	classification receiverCauseClassification,
	decision receiverAttemptDecision,
) receiverCauseClassification {
	switch decision.disposition {
	case "", ReceiverDispositionFallbackAllowed, ReceiverDispositionSessionUnavailable:
		return classification
	case ReceiverDispositionSessionUnsafe:
		classification.classes = append([]ReceiverCauseClass{ReceiverCauseProtocol}, classification.classes...)
		return classification
	default:
		// Invalid internal decisions are local adapter failures. They are observable
		// but cannot manufacture authenticated session authority.
		classification.retained = joinReceiverResiduals([]error{
			ErrProtocol,
			classification.retained,
		})
		classification.classes = append(
			[]ReceiverCauseClass{ReceiverCauseProtocol, ReceiverCauseUnknown},
			classification.classes...,
		)
		classification.complete = false
		return classification
	}
}

func joinReceiverResiduals(retained []error) error {
	var nonNil []error
	for _, cause := range retained {
		if cause != nil {
			nonNil = append(nonNil, cause)
		}
	}
	switch len(nonNil) {
	case 0:
		return nil
	case 1:
		return nonNil[0]
	default:
		return errors.Join(nonNil...)
	}
}

func receiverClassifiedCause(classification receiverCauseClassification) error {
	components := make([]error, 0, len(classification.benign)+1)
	if classification.retained != nil {
		components = append(components, classification.retained)
	}
	for _, benign := range classification.benign {
		switch benign {
		case ReceiverBenignContextCanceled:
			components = append(components, context.Canceled)
		case ReceiverBenignLocalOperationMissing, ReceiverBenignRemoteOperationMissing:
			components = append(components, session.ErrOperationMissing)
		}
	}
	return joinReceiverResiduals(components)
}

func uniqueReceiverBenignCauses(causes []ReceiverBenignCause) []ReceiverBenignCause {
	return uniqueValues(causes)
}

func uniqueReceiverCauseClasses(classes []ReceiverCauseClass) []ReceiverCauseClass {
	return uniqueValues(classes)
}

func uniqueValues[T comparable](values []T) []T {
	if len(values) < 2 {
		return values
	}
	seen := make(map[T]struct{}, len(values))
	result := make([]T, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// ReceiverCauseClasses returns text-free diagnostic classes for a receiver error.
// Session lifetime comes only from the sealed decision on ReceiverAttemptOutcome.
func ReceiverCauseClasses(cause error) []ReceiverCauseClass {
	return classifyReceiverCause(cause, receiverCausePolicy{}).classes
}

//nolint:errorlint // Matches the current error node during bounded Unwrap traversal.
func directReceiverCauseClass(cause error) (ReceiverCauseClass, bool) {
	switch cause {
	case session.ErrRuntimeClosed:
		return ReceiverCauseRuntimeClosed, true
	case ErrConfig:
		return ReceiverCauseConfiguration, true
	case session.ErrOperationMissing:
		return ReceiverCauseOperationMissing, true
	case ErrPeerNegotiationTimeout:
		return ReceiverCauseNegotiationTimeout, true
	case ErrPeerAdmissionTimeout:
		return ReceiverCauseAdmissionTimeout, true
	case errCandidateLimit:
		return ReceiverCauseCandidateLimit, true
	case errChannelAdmission:
		return ReceiverCauseChannelAdmission, true
	case ErrEventCapacity:
		return ReceiverCauseEventCapacity, true
	case ErrNegotiation:
		return ReceiverCauseNegotiation, true
	case ErrProtocol, protocolsession.ErrUnknownMessageKind, protocolsession.ErrInvalidOperationFailure:
		return ReceiverCauseProtocol, true
	case context.DeadlineExceeded:
		return ReceiverCauseDeadline, true
	case errPeerShutdown:
		return ReceiverCausePeerShutdown, true
	case errChannelDrain:
		return ReceiverCauseChannelDrain, true
	default:
		return "", false
	}
}
