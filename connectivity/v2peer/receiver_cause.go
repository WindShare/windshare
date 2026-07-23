package v2peer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

const (
	maximumReceiverErrorTreeDepth = 64
	maximumReceiverErrorTreeNodes = 1024
)

var (
	errReceiverOpaqueCause = errors.New("receiver operation retained an opaque failure")

	// Interface conformance is not traversal authority: an unaudited Error or
	// Unwrap method may panic, mutate, or conceal an uncomparable value. Only
	// these exact standard-library, core-owned, and receiver-owned
	// implementations cross the receiver's structural trust boundary.
	errReceiverStructuralProbe   = errors.New("receiver structural error probe")
	receiverTrustedLeafType      = reflect.TypeOf(errReceiverStructuralProbe)
	receiverTrustedJoinType      = reflect.TypeOf(errors.Join(errReceiverStructuralProbe, errReceiverStructuralProbe))
	receiverTrustedWrapType      = reflect.TypeOf(fmt.Errorf("receiver structural wrapper: %w", errReceiverStructuralProbe))
	receiverTrustedMultiWrapType = reflect.TypeOf(fmt.Errorf("receiver structural wrappers: %w %w", errReceiverStructuralProbe, errReceiverStructuralProbe))
	receiverSessionFailureType   = reflect.TypeFor[*transfer.SessionFailureError]()
	receiverRemoteFailureType    = reflect.TypeFor[sessionruntime.RemoteOperationError]()
	receiverSafeDiagnosticType   = reflect.TypeFor[*receiverSafeDiagnosticError]()
	receiverOwnedJoinType        = reflect.TypeFor[*receiverOwnedJoinError]()
)

// receiverOwnedJoinError makes the canonical result safe to share across
// Outcome, Err, and Close. The standard join exposes its backing slice, so a
// shallow outcome copy cannot protect the published graph.
type receiverOwnedJoinError struct {
	message  string
	children []error
}

func (failure *receiverOwnedJoinError) Error() string { return failure.message }

func (failure *receiverOwnedJoinError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return append([]error(nil), failure.children...)
}

// receiverSafeDiagnosticError keeps phase context while exposing only a
// classifier-owned residual. Reusing fmt's original wrapper would make its
// unaudited child reachable again through errors.Is.
type receiverSafeDiagnosticError struct {
	message string
	cause   error
}

func (failure *receiverSafeDiagnosticError) Error() string { return failure.message }
func (failure *receiverSafeDiagnosticError) Unwrap() error { return failure.cause }

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

type receiverSingleUnwrapper interface {
	Unwrap() error
}

type receiverMultiUnwrapper interface {
	Unwrap() []error
}

type receiverStructuralErrorKind uint8

const (
	receiverStructuralSingle receiverStructuralErrorKind = iota + 1
	receiverStructuralMulti
)

type receiverErrorTreeTraversal struct {
	remaining int
}

func newReceiverErrorTreeTraversal() *receiverErrorTreeTraversal {
	return &receiverErrorTreeTraversal{remaining: maximumReceiverErrorTreeNodes}
}

func (traversal *receiverErrorTreeTraversal) enter() bool {
	if traversal.remaining == 0 {
		return false
	}
	traversal.remaining--
	return true
}

func classifyReceiverCause(cause error, policy receiverCausePolicy) receiverCauseClassification {
	if cause == nil {
		return receiverCauseClassification{complete: true}
	}
	classification := classifyReceiverCauseAtDepth(
		cause,
		policy,
		0,
		newReceiverErrorTreeTraversal(),
	)
	classification.benign = uniqueReceiverBenignCauses(classification.benign)
	classification.classes = uniqueReceiverCauseClasses(classification.classes)
	return classification
}

func snapshotReceiverCauseWithStatus(cause error) (error, bool) {
	// Snapshot before cancellation or cross-goroutine storage. Owner-specific
	// benign filtering happens later against this already-owned graph.
	classified := classifyReceiverCause(cause, receiverCausePolicy{})
	return classified.retained, !classified.complete
}

func classifyReceiverCauseAtDepth(
	cause error,
	policy receiverCausePolicy,
	depth int,
	traversal *receiverErrorTreeTraversal,
) receiverCauseClassification {
	if classified, terminal := classifyReceiverCauseBoundary(cause, policy, depth, traversal); terminal {
		return classified
	}
	if classified, sessionFailure := classifyReceiverSessionCause(cause, depth, traversal); sessionFailure {
		return classified
	}
	if class, known := directReceiverCauseClass(cause); known {
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{class},
			complete: true,
		}
	}
	switch receiverTrustedStructure(cause) {
	case receiverStructuralMulti:
		return classifyReceiverMultiCause(cause, policy, depth, traversal)
	case receiverStructuralSingle:
		return classifyReceiverSingleCause(cause, policy, depth, traversal)
	default:
		return classifyReceiverLeafCause(cause)
	}
}

func classifyReceiverCauseBoundary(
	cause error,
	policy receiverCausePolicy,
	depth int,
	traversal *receiverErrorTreeTraversal,
) (receiverCauseClassification, bool) {
	if cause == nil {
		return receiverCauseClassification{complete: true}, true
	}
	if depth >= maximumReceiverErrorTreeDepth || !traversal.enter() {
		return incompleteReceiverCauseClassification(), true
	}
	if isNilReceiverCause(cause) {
		return opaqueReceiverCauseClassification(), true
	}
	if policy.contextCanceled && isExactReceiverSentinel(cause, context.Canceled) {
		return receiverCauseClassification{
			benign:   []ReceiverBenignCause{ReceiverBenignContextCanceled},
			complete: true,
		}, true
	}
	if isExactReceiverSentinel(cause, sessionruntime.ErrOperationMissing) && policy.operationMissing != "" {
		return receiverCauseClassification{
			benign:   []ReceiverBenignCause{policy.operationMissing},
			complete: true,
		}, true
	}
	return receiverCauseClassification{}, false
}

func classifyReceiverSessionCause(
	cause error,
	depth int,
	traversal *receiverErrorTreeTraversal,
) (receiverCauseClassification, bool) {
	if reflect.TypeOf(cause) != receiverSessionFailureType {
		return receiverCauseClassification{}, false
	}
	child, ok := unwrapReceiverTrustedSingle(cause)
	if !ok || child == nil {
		return receiverSessionFailureClassification(incompleteReceiverCauseClassification()), true
	}
	// SessionFailureError is a semantic boundary owned by core. Its child is
	// diagnostic context, so local benign filtering must not erase it.
	return receiverSessionFailureClassification(classifyReceiverCauseAtDepth(
		child,
		receiverCausePolicy{},
		depth+1,
		traversal,
	)), true
}

func classifyReceiverMultiCause(
	cause error,
	policy receiverCausePolicy,
	depth int,
	traversal *receiverErrorTreeTraversal,
) receiverCauseClassification {
	children, ok := unwrapReceiverTrustedMulti(cause)
	if !ok || len(children) == 0 {
		return incompleteReceiverCauseClassification()
	}
	var retained []error
	var benign []ReceiverBenignCause
	var classes []ReceiverCauseClass
	complete := true
	// Depth truncation is branch-local, so later siblings still receive the
	// remaining global budget. Only actual node exhaustion ends reconstruction.
	// Session authority is carried separately and never depends on this order.
	for _, child := range children {
		if traversal.remaining == 0 || child == nil {
			incomplete := incompleteReceiverCauseClassification()
			retained = append(retained, incomplete.retained)
			classes = append(classes, incomplete.classes...)
			complete = false
			break
		}
		classified := classifyReceiverCauseAtDepth(child, policy, depth+1, traversal)
		if classified.retained != nil {
			retained = append(retained, classified.retained)
		}
		benign = append(benign, classified.benign...)
		classes = append(classes, classified.classes...)
		complete = complete && classified.complete
	}
	residual := joinReceiverResiduals(retained)
	if reflect.TypeOf(cause) == receiverTrustedMultiWrapType && residual != nil {
		var diagnosticOK bool
		residual, diagnosticOK = rebuildReceiverDiagnostic(cause, residual)
		if !diagnosticOK {
			// Losing diagnostic context cannot revoke identities already proven
			// in audited children; the opaque leaf covers only that new gap.
			residual = joinReceiverResiduals([]error{residual, errReceiverOpaqueCause})
			classes = append(classes, ReceiverCauseUnknown)
			complete = false
		}
	}
	return receiverCauseClassification{
		retained: residual,
		benign:   benign,
		classes:  classes,
		complete: complete,
	}
}

func classifyReceiverSingleCause(
	cause error,
	policy receiverCausePolicy,
	depth int,
	traversal *receiverErrorTreeTraversal,
) receiverCauseClassification {
	child, ok := unwrapReceiverTrustedSingle(cause)
	if !ok || child == nil {
		return incompleteReceiverCauseClassification()
	}
	classified := classifyReceiverCauseAtDepth(child, policy, depth+1, traversal)
	if classified.retained == nil {
		return classified
	}
	residual, diagnosticOK := rebuildReceiverDiagnostic(cause, classified.retained)
	if diagnosticOK {
		classified.retained = residual
		return classified
	}
	classified.retained = joinReceiverResiduals([]error{
		classified.retained,
		errReceiverOpaqueCause,
	})
	classified.classes = append(classified.classes, ReceiverCauseUnknown)
	classified.complete = false
	return classified
}

func classifyReceiverLeafCause(cause error) receiverCauseClassification {
	switch reflect.TypeOf(cause) {
	case receiverRemoteFailureType:
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{ReceiverCauseProtocol},
			complete: true,
		}
	case receiverTrustedLeafType:
		return receiverCauseClassification{
			retained: cause,
			classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
			complete: true,
		}
	default:
		// All unaudited types remain opaque, including apparently comparable
		// values: an interface field may hold an uncomparable dynamic value.
		return opaqueReceiverCauseClassification()
	}
}

func receiverSessionFailureClassification(
	child receiverCauseClassification,
) receiverCauseClassification {
	// The exact core wrapper is useful diagnostic context only. Session lifetime
	// comes from sealed producer evidence and is never reconstructed here.
	child.classes = append([]ReceiverCauseClass{ReceiverCauseProtocol}, child.classes...)
	return child
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

func opaqueReceiverCauseClassification() receiverCauseClassification {
	return receiverCauseClassification{
		retained: errReceiverOpaqueCause,
		classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
		complete: true,
	}
}

func incompleteReceiverCauseClassification() receiverCauseClassification {
	return receiverCauseClassification{
		retained: errReceiverOpaqueCause,
		classes:  []ReceiverCauseClass{ReceiverCauseUnknown},
	}
}

func receiverTrustedStructure(cause error) receiverStructuralErrorKind {
	causeType := reflect.TypeOf(cause)
	switch causeType {
	case receiverTrustedJoinType, receiverTrustedMultiWrapType, receiverOwnedJoinType:
		return receiverStructuralMulti
	case receiverTrustedWrapType, receiverSafeDiagnosticType:
		return receiverStructuralSingle
	default:
		return 0
	}
}

func rebuildReceiverDiagnostic(cause, retained error) (rebuilt error, ok bool) {
	// A trusted fmt wrapper has already materialized its text. Copying that text
	// into an owned wrapper preserves phase context without making the filtered
	// original child graph reachable again through errors.Is or errors.As.
	defer func() {
		if recover() != nil {
			rebuilt, ok = nil, false
		}
	}()
	return &receiverSafeDiagnosticError{message: cause.Error(), cause: retained}, true
}

func unwrapReceiverTrustedSingle(cause error) (child error, ok bool) {
	defer func() {
		if recover() != nil {
			child, ok = nil, false
		}
	}()
	child = cause.(receiverSingleUnwrapper).Unwrap()
	return child, true
}

func unwrapReceiverTrustedMulti(cause error) (children []error, ok bool) {
	defer func() {
		if recover() != nil {
			children, ok = nil, false
		}
	}()
	children = cause.(receiverMultiUnwrapper).Unwrap()
	return children, true
}

func isNilReceiverCause(cause error) bool {
	value := reflect.ValueOf(cause)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func joinReceiverResiduals(retained []error) error {
	owned := make([]error, 0, len(retained))
	for _, cause := range retained {
		if cause != nil {
			owned = append(owned, cause)
		}
	}
	switch len(owned) {
	case 0:
		return nil
	case 1:
		return owned[0]
	default:
		var message strings.Builder
		for index, cause := range owned {
			if index != 0 {
				message.WriteByte('\n')
			}
			message.WriteString(cause.Error())
		}
		return &receiverOwnedJoinError{
			message:  message.String(),
			children: append([]error(nil), owned...),
		}
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
			components = append(components, sessionruntime.ErrOperationMissing)
		}
	}
	return joinReceiverResiduals(components)
}

func uniqueReceiverBenignCauses(causes []ReceiverBenignCause) []ReceiverBenignCause {
	if len(causes) < 2 {
		return causes
	}
	seen := make(map[ReceiverBenignCause]struct{}, len(causes))
	result := make([]ReceiverBenignCause, 0, len(causes))
	for _, cause := range causes {
		if _, exists := seen[cause]; exists {
			continue
		}
		seen[cause] = struct{}{}
		result = append(result, cause)
	}
	return result
}

func uniqueReceiverCauseClasses(classes []ReceiverCauseClass) []ReceiverCauseClass {
	if len(classes) < 2 {
		return classes
	}
	seen := make(map[ReceiverCauseClass]struct{}, len(classes))
	result := make([]ReceiverCauseClass, 0, len(classes))
	for _, class := range classes {
		if _, exists := seen[class]; exists {
			continue
		}
		seen[class] = struct{}{}
		result = append(result, class)
	}
	return result
}

// ReceiverCauseClasses returns bounded, text-free diagnostics for an arbitrary
// error tree. It deliberately avoids errors.Is so malformed cyclic wrappers
// cannot recurse without a limit. Session lifetime comes only from the
// sealed producer decision on ReceiverAttemptOutcome.
func ReceiverCauseClasses(cause error) []ReceiverCauseClass {
	return classifyReceiverCause(cause, receiverCausePolicy{}).classes
}

func directReceiverCauseClass(cause error) (ReceiverCauseClass, bool) {
	switch {
	case isExactReceiverSentinel(cause, sessionruntime.ErrRuntimeClosed):
		return ReceiverCauseRuntimeClosed, true
	case isExactReceiverSentinel(cause, ErrConfig):
		return ReceiverCauseConfiguration, true
	case isExactReceiverSentinel(cause, sessionruntime.ErrOperationMissing):
		return ReceiverCauseOperationMissing, true
	case isExactReceiverSentinel(cause, errAttemptTimeout):
		return ReceiverCauseAttemptTimeout, true
	case isExactReceiverSentinel(cause, errCandidateLimit):
		return ReceiverCauseCandidateLimit, true
	case isExactReceiverSentinel(cause, errChannelAdmission):
		return ReceiverCauseChannelAdmission, true
	case isExactReceiverSentinel(cause, ErrEventCapacity):
		return ReceiverCauseEventCapacity, true
	case isExactReceiverSentinel(cause, ErrNegotiation):
		return ReceiverCauseNegotiation, true
	case isExactReceiverSentinel(cause, ErrProtocol),
		isExactReceiverSentinel(cause, protocolsession.ErrUnknownMessageKind),
		isExactReceiverSentinel(cause, protocolsession.ErrInvalidOperationFailure):
		return ReceiverCauseProtocol, true
	case isExactReceiverSentinel(cause, context.DeadlineExceeded):
		return ReceiverCauseDeadline, true
	case isExactReceiverSentinel(cause, errPeerShutdown):
		return ReceiverCausePeerShutdown, true
	case isExactReceiverSentinel(cause, errChannelDrain):
		return ReceiverCauseChannelDrain, true
	default:
		return "", false
	}
}

func isExactReceiverSentinel(cause, sentinel error) bool {
	causeValue := reflect.ValueOf(cause)
	sentinelValue := reflect.ValueOf(sentinel)
	return causeValue.IsValid() && sentinelValue.IsValid() &&
		causeValue.Type() == sentinelValue.Type() &&
		causeValue.Comparable() && causeValue.Equal(sentinelValue)
}
