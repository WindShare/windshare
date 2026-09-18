package task

import "errors"

var ErrInvalidSettlement = errors.New("task result violates its settlement contract")

// Settlement is the workflow's authoritative decision after resource cleanup.
// Both Wait and lifecycle observers receive this same decision; neither infers
// an outcome from a stop request or the presence of a low-level cause.
type Settlement struct {
	Outcome      Outcome
	FailureClass FailureClass
	// Cancellation may retain its cause without becoming a business failure.
	// Outcome, rather than Err alone, distinguishes those terminal states.
	Err          error
	CleanupError error
}

func (settlement Settlement) Valid() bool {
	if settlement.FailureClass > FailureSourceDrift || settlement.CleanupError != nil && settlement.Outcome != OutcomeFailed {
		return false
	}
	switch settlement.Outcome {
	case OutcomeSuccess, OutcomeStopped:
		return settlement.FailureClass == FailureNone && settlement.Err == nil
	case OutcomeCancelled:
		return settlement.FailureClass == FailureNone
	case OutcomePartial, OutcomePaused, OutcomeFailed:
		return settlement.FailureClass != FailureNone && settlement.Err != nil
	default:
		return false
	}
}

func (settlement Settlement) checked() Settlement {
	if settlement.Valid() {
		return settlement
	}
	return Settlement{
		Outcome: OutcomeFailed, FailureClass: FailureLocal,
		Err:          errors.Join(ErrInvalidSettlement, settlement.Err, settlement.CleanupError),
		CleanupError: settlement.CleanupError,
	}
}
