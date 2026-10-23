package failurestate

import (
	"errors"

	"github.com/windshare/windshare/core/transfer/fault"
)

func Join(candidates ...error) error {
	return Err(Reduce(candidates...))
}

func JoinClosed(candidates ...*Failure) *Failure {
	errorsToReduce := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil {
			errorsToReduce = append(errorsToReduce, candidate)
		}
	}
	return Reduce(errorsToReduce...)
}

func Merge(
	current *Failure,
	candidates ...error,
) *Failure {
	reduced := Reduce(candidates...)
	switch {
	case current == nil:
		return reduced
	case reduced == nil:
		return current
	default:
		return JoinClosed(current, reduced)
	}
}

func Reduce(candidates ...error) *Failure {
	var joined fault.Fault
	var cancellation cancellationKind
	var soleDirect *Failure
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
			if exact := exactCancellation(candidate); exact != cancellationNone {
				failure = &Failure{
					cancellation: exact, diagnostic: candidate,
				}
			} else {
				failure = DependencyContract(candidate)
			}
		}
		joined = fault.Join(joined, failure.value)
		if failure.Canceled() {
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
	return &Failure{
		value:        joined,
		cancellation: cancellation,
		diagnostic:   errors.Join(diagnostics...),
	}
}
