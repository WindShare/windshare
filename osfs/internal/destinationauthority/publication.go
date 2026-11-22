package destinationauthority

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// PublishFileNoReplace is intentionally root-level. Descendant publication is
// delegated to a separately retained directory authority; neither path text nor
// DestinationBindingID can substitute for that handle.
func (authority *BoundDestination) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	if source == nil || name == "" {
		return 0, ErrInvalidReservation
	}
	var outcome outputcap.PublishNoReplaceOutcome
	err := authority.withGuardedRoot(func(root outputcap.Directory) error {
		publisher, ok := root.(fileNoReplacePublisher)
		if !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("file publication is unavailable"))
		}
		var err error
		outcome, err = publisher.PublishFileNoReplace(source, name)
		return err
	})
	if err != nil {
		if outcome == outputcap.PublishNoReplaceCommitted || outcome == outputcap.PublishNoReplaceCollision {
			return outputcap.PublishNoReplaceIndeterminate, errors.Join(ErrReservationIndeterminate, err)
		}
		return outcome, err
	}
	if !outcome.Valid() {
		return 0, ErrReservationIndeterminate
	}
	return outcome, nil
}
