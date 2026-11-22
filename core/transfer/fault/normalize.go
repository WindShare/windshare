package fault

import (
	"context"
	"errors"
	"fmt"
)

type BoundaryResultKind uint8

const (
	BoundarySuccess BoundaryResultKind = iota
	BoundaryCanceled
	BoundaryFailed
)

// BoundaryResult is a closed normalization result. It cannot carry the raw
// cause, so settlement code receives no error graph to reinterpret.
type BoundaryResult struct {
	kind  BoundaryResultKind
	fault Fault
}

func (result BoundaryResult) Kind() BoundaryResultKind { return result.kind }

func (result BoundaryResult) Fault() (Fault, bool) {
	return result.fault, result.kind == BoundaryFailed && result.fault.Valid()
}

// BoundaryError is the single normalized error carrier shared across transfer
// boundaries, lane scheduling, and job lifecycle policy. Fault and cancellation
// stay independent so a sibling cancellation never erases the fault that caused
// the owning operation to stop.
type BoundaryError struct {
	fault        Fault
	cancellation error
	cause        error
}

// Wrap wraps cause with a validated transfer fault for collaborator boundaries.
func Wrap(value Fault, cause error) error {
	if !value.Valid() {
		return ErrInvalidFault
	}
	return &BoundaryError{fault: value, cause: cause}
}

// New constructs a normalized BoundaryError, failing closed to
// DependencyContractFault if value is invalid.
func New(value Fault, cause error) *BoundaryError {
	if !value.Valid() {
		value = DependencyContractFault()
	}
	return &BoundaryError{fault: value, cause: cause}
}

// DependencyContract returns the fail-closed dependency-contract BoundaryError.
func DependencyContract(cause error) *BoundaryError {
	return New(DependencyContractFault(), cause)
}

func (failure *BoundaryError) Error() string {
	if failure == nil || (!failure.fault.Valid() && failure.cancellation == nil) {
		return ErrInvalidFault.Error()
	}
	switch {
	case failure.Canceled() && failure.fault.Valid() && failure.cause != nil:
		return fmt.Sprintf("transfer canceled with fault %s: %v", failure.fault, failure.cause)
	case failure.Canceled() && failure.cause != nil:
		return fmt.Sprintf("transfer canceled: %v", failure.cause)
	case failure.Canceled():
		return "transfer canceled"
	case failure.cause == nil:
		return fmt.Sprintf("transfer boundary fault: %s", failure.fault)
	default:
		return fmt.Sprintf("transfer boundary fault: %s: %v", failure.fault, failure.cause)
	}
}

func (failure *BoundaryError) Unwrap() error {
	if failure == nil {
		return nil
	}
	// When cancellation is authoritative, expose only the winning cancellation
	// sentinel so errors.Is cannot match a suppressed cancellation in cause.
	if failure.cancellation != nil {
		return failure.cancellation
	}
	return failure.cause
}

func (failure *BoundaryError) Fault() Fault {
	if failure == nil {
		return Fault{}
	}
	return failure.fault
}

func (failure *BoundaryError) Canceled() bool {
	return failure != nil && failure.cancellation != nil
}

func (failure *BoundaryError) Cancellation() error {
	if failure == nil {
		return nil
	}
	return failure.cancellation
}

func (failure *BoundaryError) Cause() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

// Err converts a possibly-nil *BoundaryError to an untyped error without
// producing a typed-nil interface value.
func (failure *BoundaryError) Err() error {
	if failure == nil {
		return nil
	}
	return failure
}

// Project returns an error suitable for returning across a collaborator port,
// preserving cancellation as the bare sentinel and faults as *BoundaryError.
func (failure *BoundaryError) Project(diagnostic error) error {
	if failure == nil {
		return Wrap(DependencyContractFault(), diagnostic)
	}
	if failure.cancellation != nil {
		return failure.cancellation
	}
	return Wrap(failure.fault, diagnostic)
}

// Admit accepts only a direct *BoundaryError carrier, rejecting arbitrary
// external wrappers from acquiring closed policy authority.
func Admit(err error) (*BoundaryError, bool) {
	//nolint:errorlint
	failure, ok := err.(*BoundaryError)
	if !ok || failure == nil || (!failure.fault.Valid() && failure.cancellation == nil) {
		return nil, false
	}
	return failure, true
}

// Require ensures any non-nil error is a direct *BoundaryError, failing closed
// to DependencyContract for unadmitted values.
func Require(err error) *BoundaryError {
	if err == nil {
		return nil
	}
	if failure, ok := Admit(err); ok {
		return failure
	}
	return DependencyContract(err)
}

// FaultOf returns the Fault carried by a direct *BoundaryError, or zero.
func FaultOf(err error) Fault {
	failure, ok := Admit(err)
	if !ok {
		return Fault{}
	}
	return failure.fault
}

// CauseOf returns the diagnostic cause of a direct *BoundaryError, or err itself.
func CauseOf(err error) error {
	failure, ok := Admit(err)
	if !ok {
		return err
	}
	return failure.cause
}

// ContextCause returns an owned *BoundaryError propagated via context.WithCancelCause.
func ContextCause(ctx context.Context) *BoundaryError {
	if ctx == nil {
		return nil
	}
	failure, _ := Admit(context.Cause(ctx))
	return failure
}

// BoundaryCancellation returns a standard cancellation sentinel for an
// unclassified collaborator result or an explicitly canceled BoundaryError. A
// classified fault without explicit cancellation owns its outcome; cancellation
// inside its diagnostic cause cannot grant cancellation authority.
func BoundaryCancellation(err error) error {
	if err == nil {
		return nil
	}
	if classified, ok := errors.AsType[*BoundaryError](err); ok && classified != nil {
		if classified.cancellation != nil {
			return classified.cancellation
		}
		if classified.fault.Valid() {
			return nil
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func boundaryCancellation(ctx context.Context, err error) error {
	if ctx != nil {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			return context.DeadlineExceeded
		case context.Canceled:
			return context.Canceled
		}
	}
	return BoundaryCancellation(err)
}

// IsCancellation reports whether caller context or an immediate collaborator
// result represents cancellation rather than a classified fault.
func IsCancellation(ctx context.Context, err error) bool {
	return boundaryCancellation(ctx, err) != nil
}

// FromCancellation builds a canceled *BoundaryError from ctx or diagnostic,
// preserving any already-owned *BoundaryError intact.
func FromCancellation(ctx context.Context, diagnostic error) *BoundaryError {
	if existing, ok := Admit(diagnostic); ok {
		return existing
	}
	if existing := ContextCause(ctx); existing != nil {
		return existing
	}
	cancellation := boundaryCancellation(ctx, diagnostic)
	if cancellation == nil {
		return DependencyContract(diagnostic)
	}
	if diagnostic == nil && ctx != nil {
		diagnostic = context.Cause(ctx)
	}
	return &BoundaryError{cancellation: cancellation, cause: diagnostic}
}

// Normalize admits one collaborator error at a boundary into *BoundaryError.
// Caller cancellation and propagated worker causes win before inspecting err.
func Normalize(ctx context.Context, err error, classify func(error) (Fault, bool)) *BoundaryError {
	if existing := ContextCause(ctx); existing != nil {
		return existing
	}
	if cancellation := boundaryCancellation(ctx, err); cancellation != nil {
		diagnostic := err
		if diagnostic == nil && ctx != nil {
			diagnostic = context.Cause(ctx)
		}
		return &BoundaryError{cancellation: cancellation, cause: diagnostic}
	}
	if err == nil {
		return nil
	}
	if direct, ok := Admit(err); ok {
		return direct
	}
	if wrapped, ok := errors.AsType[*BoundaryError](err); ok && wrapped != nil && wrapped.fault.Valid() {
		return &BoundaryError{fault: wrapped.fault, cancellation: wrapped.cancellation, cause: err}
	}
	if classify != nil {
		if value, known := classify(err); known {
			return New(value, err)
		}
	}
	return DependencyContract(err)
}

// Reduce combines direct *BoundaryError carriers and bare cancellation sentinels
// without traversing arbitrary wrapper graphs. A singleton direct carrier is
// returned unchanged to preserve context.WithCancelCause identity.
func Reduce(candidates ...error) *BoundaryError {
	var joined Fault
	var cancellation error
	var soleDirect *BoundaryError
	candidateCount := 0
	diagnostics := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		candidateCount++
		failure, ok := Admit(candidate)
		if candidateCount == 1 && ok {
			soleDirect = failure
		} else {
			soleDirect = nil
		}
		if !ok {
			switch candidate {
			case context.DeadlineExceeded:
				failure = &BoundaryError{cancellation: context.DeadlineExceeded, cause: candidate}
			case context.Canceled:
				failure = &BoundaryError{cancellation: context.Canceled, cause: candidate}
			default:
				failure = DependencyContract(candidate)
			}
		}
		joined = Join(joined, failure.fault)
		if failure.cancellation != nil {
			cancellation = joinCancellation(cancellation, failure.cancellation)
		}
		if failure.cause != nil {
			diagnostics = append(diagnostics, failure.cause)
		}
	}
	if candidateCount == 1 && soleDirect != nil {
		return soleDirect
	}
	if !joined.Valid() && cancellation == nil {
		return nil
	}
	return &BoundaryError{
		fault:        joined,
		cancellation: cancellation,
		cause:        errors.Join(diagnostics...),
	}
}

// Merge joins candidates into current, preserving singleton identity when one side is nil.
func Merge(current *BoundaryError, candidates ...error) *BoundaryError {
	reduced := Reduce(candidates...)
	switch {
	case current == nil:
		return reduced
	case reduced == nil:
		return current
	default:
		return Reduce(current, reduced)
	}
}

// JoinErrors reduces candidates and returns a typed-nil-safe error.
func JoinErrors(candidates ...error) error {
	return Reduce(candidates...).Err()
}

func joinCancellation(left, right error) error {
	if left == context.DeadlineExceeded || right == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	if right != nil {
		return right
	}
	return left
}

// NormalizeBoundary checks caller cancellation before accepting a typed collaborator
// fault. An unknown error is a local dependency-contract breach and therefore
// pauses output authority rather than acquiring file isolation or retirement
// authority.
func NormalizeBoundary(ctx context.Context, err error) BoundaryResult {
	if ctx.Err() != nil {
		return BoundaryResult{kind: BoundaryCanceled}
	}
	return NormalizeBoundaryError(err)
}

// NormalizeBoundaryError is the context-free form for a completed collaborator
// result. Keeping it separate prevents locked invariant and trace paths from
// inventing a context while retaining unclassified collaborator cancellation.
func NormalizeBoundaryError(err error) BoundaryResult {
	if err == nil {
		return BoundaryResult{kind: BoundarySuccess}
	}
	if BoundaryCancellation(err) != nil {
		return BoundaryResult{kind: BoundaryCanceled}
	}
	if normalized, ok := errors.AsType[*BoundaryError](err); ok && normalized != nil && normalized.fault.Valid() {
		return BoundaryResult{kind: BoundaryFailed, fault: normalized.fault}
	}
	return BoundaryResult{kind: BoundaryFailed, fault: DependencyContractFault()}
}

// ReduceBoundaryErrors combines immediate sibling collaborator results without
// deriving policy from their aggregate error topology. Each candidate is
// normalized once, Fault values use the total deterministic join, and any
// cancellation remains outside the fault lattice.
func ReduceBoundaryErrors(ctx context.Context, candidates ...error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ReduceBoundaryErrorSet(candidates...)
}

// ReduceBoundaryErrorSet joins already-completed sibling results when no
// operation context exists. It preserves explicit cancellation outside the
// fault lattice and applies the same total Fault join as the context-aware form.
func ReduceBoundaryErrorSet(candidates ...error) error {
	var joined Fault
	var cancellation error
	diagnostics := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		diagnostics = append(diagnostics, candidate)
		result := NormalizeBoundaryError(candidate)
		switch result.Kind() {
		case BoundaryCanceled:
			cancellation = joinCancellation(cancellation, BoundaryCancellation(candidate))
		case BoundaryFailed:
			value, _ := result.Fault()
			joined = Join(joined, value)
		}
	}
	if cancellation != nil {
		return cancellation
	}
	if !joined.Valid() {
		return nil
	}
	return Wrap(joined, errors.Join(diagnostics...))
}

func DependencyContractFault() Fault {
	// A private literal makes the fail-closed default total even if this function
	// is needed while already handling a collaborator contract breach.
	return Fault{
		domain: DomainSession,
		scope:  ScopeOutputPause,
		code:   uint16(SessionDependencyContract),
	}
}

type Retirement uint8

const (
	RetirementPermanentSource Retirement = iota + 1
	RetirementInvalidatedRevision
)

// RetirementFor is an explicit allowlist. Severity alone never permits state
// deletion, and the file-local check prevents a wider source failure from being
// repurposed as authority over one file.
func RetirementFor(fault Fault) (Retirement, bool) {
	if fault.domain != DomainSource || fault.scope != ScopeFileLocal {
		return 0, false
	}
	switch SourceCode(fault.code) {
	case SourcePermanent:
		return RetirementPermanentSource, true
	case SourceRevisionInvalidated:
		return RetirementInvalidatedRevision, true
	default:
		return 0, false
	}
}
