package resumeauthority

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

// PinnedCheckpoint is the only private-store capability exposed to the public
// path observer. SameOwnedFile compares a live public file with the retained
// owned anchor and therefore returns Exact, Replaced, or Ambiguous, never
// Absent. Internal names and handles never cross this boundary.
type PinnedCheckpoint interface {
	Record() checkpointmodel.Record
	SameOwnedFile(context.Context, outputcap.File) (Evidence, error)
}

// PinnedCheckpointProvider resolves only observations from the leased snapshot.
// Implementations must reject calls before Observe and after Close.
type PinnedCheckpointProvider interface {
	PinnedCheckpoint(checkpointmodel.RecordID) (PinnedCheckpoint, bool)
}

// PublicationObserver owns guarded public-path traversal. A returned pin keeps
// enough exact lineage and final-entry identity to detect replacement around
// every private checkpoint mutation.
type PublicationObserver interface {
	PinPublication(context.Context, PinnedCheckpoint) (PinnedPublication, error)
}

type PinnedPublication interface {
	Observation() PublicationObservation
	Revalidate(context.Context) (Evidence, error)
	Close() error
}

type publicationRepository interface {
	LeasedRepository
	PinPublications(context.Context, RepositorySnapshot) ([]PinnedPublication, error)
}

func closePinnedPublications(publications []PinnedPublication) error {
	errs := make([]error, 0, len(publications))
	for _, publication := range slices.Backward(publications) {
		if publication != nil {
			errs = append(errs, publication.Close())
		}
	}
	return errors.Join(errs...)
}
