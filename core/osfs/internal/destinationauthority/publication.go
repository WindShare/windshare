package destinationauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

// LiveCleanupStageParent supplies only the exact, freshly revalidated final
// parent needed for one native stage-creation cut. The protected journal and
// proof namespace remain owned by BoundDestination.
type LiveCleanupStageParent interface {
	WithExactParent(context.Context, func(outputcap.Directory) error) error
}

// PublishFileNoReplace is intentionally root-level. Descendant publication is
// delegated to a separately retained directory authority; neither path text nor
// DestinationAuthorityID can substitute for that handle.
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

// CreateLiveCleanupStage enforces record-before-stage by accepting only an
// already committed ticket and using the retained authenticated proof namespace.
func (authority *BoundDestination) CreateLiveCleanupStage(
	ctx context.Context,
	parent LiveCleanupStageParent,
	ticket checkpointmodel.LiveCleanupTicket,
) (outputcap.MutableFile, checkpointmodel.LiveCleanupTicket, error) {
	if authority == nil || ctx == nil || parent == nil || !ticket.Valid() ||
		ticket.State() != checkpointmodel.LiveCleanupTicketCommitted {
		return nil, checkpointmodel.LiveCleanupTicket{}, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, err
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed || authority.proof == nil || authority.journal.journal == nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, ErrAuthorityClosed
	}
	if ticket.Profile() != authority.profile {
		return nil, checkpointmodel.LiveCleanupTicket{}, ErrInvalidConfiguration
	}
	var stage outputcap.MutableFile
	var createdTicket checkpointmodel.LiveCleanupTicket
	if err := authority.journal.journal.Create(ticket); err != nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, err
	}
	err := parent.WithExactParent(ctx, func(finalParent outputcap.Directory) error {
		creator, ok := finalParent.(liveCleanupStageCreator)
		if !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("live-cleanup stage creation is unavailable"))
		}
		if err := creator.CreateLiveCleanupStage(authority.proof, ticket); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, err
	}
	opened, err := authority.proof.OpenMutableFile(ticket.StageName(), false)
	if err != nil || opened == nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, errors.Join(
			ErrReservationIndeterminate, err, closeFile(opened),
		)
	}
	size, err := opened.Size()
	if err != nil || size != ticket.ExactSize() {
		return nil, checkpointmodel.LiveCleanupTicket{}, errors.Join(
			ErrReservationIndeterminate, err, opened.Close(),
		)
	}
	next, err := checkpointmodel.ReduceLiveCleanupTicket(
		ticket, checkpointmodel.LiveCleanupRecordStageCreated,
	)
	if err != nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, errors.Join(
			ErrReservationIndeterminate, err, opened.Close(),
		)
	}
	if err := authority.journal.journal.Replace(ticket, next); err != nil {
		return nil, checkpointmodel.LiveCleanupTicket{}, errors.Join(
			ErrReservationIndeterminate, err, opened.Close(),
		)
	}
	stage = opened
	createdTicket = next
	return stage, createdTicket, nil
}

// RemoveLiveCleanupStage closes the record-before-stage protocol only after the
// exact protected name was durably removed. A definite no-replace collision may
// clean that owned stage; publication ambiguity must retain both ticket and stage
// as reconciliation evidence.
func (authority *BoundDestination) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	if authority == nil || expected == nil || !ticket.Valid() ||
		ticket.State() != checkpointmodel.LiveCleanupStageCreated {
		return ErrInvalidConfiguration
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed || authority.proof == nil || authority.journal.journal == nil {
		return ErrAuthorityClosed
	}
	if ticket.Profile() != authority.profile {
		return ErrInvalidConfiguration
	}
	remover, ok := authority.proof.(liveCleanupStageRemover)
	if !ok {
		return errors.Join(ErrInvalidConfiguration, errors.New("live-cleanup stage removal is unavailable"))
	}
	if err := remover.RemoveLiveCleanupStage(ticket, expected); err != nil {
		return err
	}
	removed, err := checkpointmodel.ReduceLiveCleanupTicket(
		ticket, checkpointmodel.LiveCleanupRecordStageRemoved,
	)
	if err != nil {
		return err
	}
	if err := authority.journal.journal.Replace(ticket, removed); err != nil {
		return err
	}
	return authority.journal.journal.Delete(removed)
}
