package lanes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/internal/failurestate"
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

type laneFailureSet struct {
	failure    *failurestate.Failure
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
		diagnostic := fmt.Errorf(
			"lane %d/%d: %w", result.state.identity.ID, result.state.identity.Epoch, result.err,
		)
		failure := result.normalized
		if result.notAdmitted {
			failure = sourceUnavailableFailure(diagnostic)
		}
		current.failure = failurestate.JoinClosed(current.failure, failure)
		current.diagnostic = errors.Join(current.diagnostic, diagnostic)
		reassignable = reassignable && result.reassignable
	}
	return current, reassignable
}
func normalizeSourceBoundary(ctx context.Context, err error) error {
	return failurestate.Err(failurestate.Normalize(ctx, err, func(cause error) (fault.Fault, bool) {
		if value, known := failurestate.ContentFault(cause); known {
			return value, true
		}
		if errors.Is(cause, ErrClosed) || failurestate.SessionTerminal(cause) {
			value, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionTransport)
			return value, true
		}
		return fault.Fault{}, false
	}))
}

func sourceUnavailableFailure(cause error) *failurestate.Failure {
	value, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourceUnavailable)
	return failurestate.New(value, cause)
}
