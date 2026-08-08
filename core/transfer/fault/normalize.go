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

// BoundaryError is the standard typed handoff from a collaborator adapter. Its
// cause remains available at that immediate boundary for diagnostics only.
type BoundaryError struct {
	fault Fault
	cause error
}

func Wrap(fault Fault, cause error) error {
	if !fault.Valid() {
		return ErrInvalidFault
	}
	return &BoundaryError{fault: fault, cause: cause}
}

func (failure *BoundaryError) Error() string {
	if failure == nil || !failure.fault.Valid() {
		return ErrInvalidFault.Error()
	}
	if failure.cause == nil {
		return fmt.Sprintf("transfer boundary fault: %s", failure.fault)
	}
	return fmt.Sprintf("transfer boundary fault: %s: %v", failure.fault, failure.cause)
}

func (failure *BoundaryError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *BoundaryError) Fault() Fault {
	if failure == nil {
		return Fault{}
	}
	return failure.fault
}

// NormalizeBoundary checks cancellation before accepting a typed collaborator
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
// inventing a context while retaining cancellation carried by the error itself.
func NormalizeBoundaryError(err error) BoundaryResult {
	if err == nil {
		return BoundaryResult{kind: BoundarySuccess}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return BoundaryResult{kind: BoundaryCanceled}
	}
	var normalized *BoundaryError
	if errors.As(err, &normalized) && normalized != nil && normalized.fault.Valid() {
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
			if cancellation == nil || errors.Is(candidate, context.DeadlineExceeded) {
				cancellation = context.Canceled
				if errors.Is(candidate, context.DeadlineExceeded) {
					cancellation = context.DeadlineExceeded
				}
			}
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
