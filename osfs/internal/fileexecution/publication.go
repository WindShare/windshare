package fileexecution

import "context"

// settlePublication keeps all placement-sensitive work within one borrowed
// capability. A completed callback includes namespace durability; readability
// alone never authorizes the published checkpoint.
func settlePublication(
	ctx context.Context,
	destination FileDestination,
	owned OwnedFile,
	expectation FinalExpectation,
	retry bool,
) (final FinalObservation, err error) {
	err = destination.WithPublication(ctx, owned, expectation, func(publication FilePublication) error {
		if publication == nil {
			return ErrPortContract
		}
		var observationErr error
		if retry {
			// PublishNoReplace also reconciles an existing exact owned final. An
			// earlier presence check would be redundant and open another race.
			final, observationErr = publication.PublishNoReplace(ctx)
		} else {
			final, observationErr = publication.Observe(ctx)
		}
		if observationErr != nil {
			return observationErr
		}
		if !final.valid() {
			return ErrInvalidObservation
		}
		if final.Condition() == FinalMatchesPublication {
			return publication.Sync(ctx)
		}
		return nil
	})
	return final, err
}
