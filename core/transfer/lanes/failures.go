package lanes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/transfer/fault"
)

// demandNotAdmittedError is an opaque concrete capability proving that a lane
// failed before its operation reached a transport. It requires explicit
// construction through NewDemandNotAdmitted; accepting an incidental public
// marker would let unrelated errors accidentally authorize duplicate work.
type demandNotAdmittedError struct{ cause error }

func NewDemandNotAdmitted(cause error) error {
	if cause == nil {
		cause = ErrInvalid
	}
	return &demandNotAdmittedError{cause: cause}
}

func (e *demandNotAdmittedError) Error() string {
	return fmt.Sprintf("demand was not admitted: %v", e.cause)
}

func (e *demandNotAdmittedError) Unwrap() error { return e.cause }

func isDemandNotAdmitted(err error) bool {
	var notAdmitted *demandNotAdmittedError
	return errors.As(err, &notAdmitted) && notAdmitted != nil
}

// demandReassignableAfterRetirementError is an opaque proof that an admitted
// block operation has acquired its exact-generation cancellation tombstone.
// Block reads are immutable under their revision lease, so a different lane may
// now issue a fresh operation without aliasing responses from the retired one.
type demandReassignableAfterRetirementError struct{ cause error }

func NewDemandReassignableAfterRetirement(cause error) error {
	if cause == nil {
		cause = ErrInvalid
	}
	return &demandReassignableAfterRetirementError{cause: cause}
}

func (e *demandReassignableAfterRetirementError) Error() string {
	return fmt.Sprintf("demand is reassignable after retiring its admitted operation: %v", e.cause)
}

func (e *demandReassignableAfterRetirementError) Unwrap() error { return e.cause }

func isDemandReassignableAfterRetirement(err error) bool {
	var retired *demandReassignableAfterRetirementError
	return errors.As(err, &retired) && retired != nil
}

// demandRetryableAfterRetirementError is an opaque proof that the peer declined
// to admit an immutable block read (for example because a delayed request fell
// behind its replay window) and the old operation is retired. The lane itself
// remains healthy, permitting a bounded fresh-operation retry even on the same
// live lane while still letting a faster lane take over when available.
type demandRetryableAfterRetirementError struct{ cause error }

func NewDemandRetryableAfterRetirement(cause error) error {
	if cause == nil {
		cause = ErrInvalid
	}
	return &demandRetryableAfterRetirementError{cause: cause}
}

func (e *demandRetryableAfterRetirementError) Error() string {
	return fmt.Sprintf("demand is retryable with a fresh operation after retirement: %v", e.cause)
}

func (e *demandRetryableAfterRetirementError) Unwrap() error { return e.cause }

func isDemandRetryableAfterRetirement(err error) bool {
	var retryable *demandRetryableAfterRetirementError
	return errors.As(err, &retryable) && retryable != nil
}

type laneFailureSet struct {
	failure    *fault.BoundaryError
	diagnostic error
}

func laneResultsReassignable(results []laneResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if !result.reassignable {
			return false
		}
	}
	return true
}

func reduceLaneFailures(current laneFailureSet, results []laneResult) (laneFailureSet, bool) {
	ordered := slices.Clone(results)
	slices.SortFunc(ordered, func(left, right laneResult) int {
		if compared := cmp.Compare(left.state.identity.ID, right.state.identity.ID); compared != 0 {
			return compared
		}
		return cmp.Compare(left.state.identity.Epoch, right.state.identity.Epoch)
	})
	reassignable := true
	for _, result := range ordered {
		if _, waiting := errors.AsType[*framechannel.BandwidthWaitError](result.err); waiting && result.reassignable {
			continue // Renewable congestion must not accumulate an unbounded error chain.
		}
		diagnostic := fmt.Errorf(
			"lane %d/%d: %w", result.state.identity.ID, result.state.identity.Epoch, result.err,
		)
		failure := result.normalized
		if result.notAdmitted {
			failure = sourceUnavailableFailure(diagnostic)
		}
		current.failure = fault.Merge(current.failure, failure.Err())
		current.diagnostic = errors.Join(current.diagnostic, diagnostic)
		reassignable = reassignable && result.reassignable
	}
	return current, reassignable
}
func normalizeSourceBoundary(ctx context.Context, err error) error {
	return fault.Normalize(ctx, err, func(cause error) (fault.Fault, bool) {
		if value, known := fault.ClassifyContent(cause); known {
			return value, true
		}
		if errors.Is(cause, ErrClosed) || fault.IsSessionTerminal(cause) {
			value, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionTransport)
			return value, true
		}
		return fault.Fault{}, false
	}).Err()
}

func sourceUnavailableFailure(cause error) *fault.BoundaryError {
	value, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceUnavailable)
	return fault.New(value, cause)
}

func exhaustedRouteFailure() laneFailureSet {
	value, _ := fault.NewSession(fault.ScopeOutputPause, fault.SessionTransport)
	return laneFailureSet{fault.New(value, ErrNoContentRoute), ErrNoContentRoute}
}
