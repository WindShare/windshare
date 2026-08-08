package resumeauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

// List constructs the live capability inventory over one already-open native
// repository. Repository ownership transfers into the inventory on success.
func List(ctx context.Context, repository Repository) (*Inventory, error) {
	if ctx == nil || repository == nil {
		return nil, ErrInvalidContract
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pinned, err := repository.ListResumeState(ctx)
	if err != nil {
		return nil, err
	}
	return NewInventory(pinned)
}

// Discard consumes one live reference, obtains the selected intent lease, and
// keeps the canonical record as the final private witness removed. Publication
// pins are checked on both sides of every store action so a changed public name
// can never silently authorize subsequent cleanup.
func Discard(ctx context.Context, reference Reference) (
	result DiscardResult,
	resultErr error,
) {
	if ctx == nil {
		return DiscardResult{}, ErrInvalidContract
	}
	claim, err := consumeReference(ctx, reference)
	if err != nil {
		return DiscardResult{}, err
	}
	defer claim.Release()

	leased, err := claim.Acquire(ctx)
	if err != nil {
		return DiscardResult{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, leased.Close()) }()

	prepared, err := prepareDiscard(ctx, leased)
	if err != nil {
		return DiscardResult{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closePinnedPublications(prepared.publications))
	}()
	if prepared.plan.ExpectedStatus() != Discarded {
		return discardPlanSettlement(prepared.plan, 0)
	}
	return applyDiscardPlan(ctx, leased, prepared.plan, prepared.publicationByRecord)
}

type preparedDiscard struct {
	plan                DiscardPlan
	publications        []PinnedPublication
	publicationByRecord map[checkpointmodel.RecordID]PinnedPublication
}

func prepareDiscard(ctx context.Context, leased LeasedRepository) (preparedDiscard, error) {
	snapshot, err := leased.Observe(ctx)
	if err != nil {
		return preparedDiscard{}, err
	}
	if snapshot.NamespaceEvidence() != EvidenceExact || len(snapshot.Attention()) > 0 {
		return preparedDiscard{plan: ReduceDiscard(snapshot, nil)}, nil
	}
	native, ok := leased.(publicationRepository)
	if !ok {
		return preparedDiscard{}, ErrInvalidContract
	}
	publications, err := native.PinPublications(ctx, snapshot)
	if err != nil {
		return preparedDiscard{}, errors.Join(err, closePinnedPublications(publications))
	}
	observations, publicationByRecord, duplicate := indexPinnedPublications(publications)
	if duplicate.Valid() {
		return preparedDiscard{
			plan:                attentionPlan([]Attention{duplicate}),
			publications:        publications,
			publicationByRecord: publicationByRecord,
		}, nil
	}
	return preparedDiscard{
		plan:                ReduceDiscard(snapshot, observations),
		publications:        publications,
		publicationByRecord: publicationByRecord,
	}, nil
}

func indexPinnedPublications(
	publications []PinnedPublication,
) (
	[]PublicationObservation,
	map[checkpointmodel.RecordID]PinnedPublication,
	Attention,
) {
	observations := make([]PublicationObservation, len(publications))
	publicationByRecord := make(map[checkpointmodel.RecordID]PinnedPublication, len(publications))
	for index, publication := range publications {
		if publication == nil {
			return nil, publicationByRecord, derivedAttention(AttentionAmbiguousPublication, nil)
		}
		observation := publication.Observation()
		observations[index] = observation
		if _, duplicate := publicationByRecord[observation.RecordID()]; duplicate {
			return nil, publicationByRecord, derivedAttention(
				AttentionAmbiguousPublication,
				observation.RecordID().Bytes(),
			)
		}
		publicationByRecord[observation.RecordID()] = publication
	}
	return observations, publicationByRecord, Attention{}
}

func applyDiscardPlan(
	ctx context.Context,
	leased LeasedRepository,
	plan DiscardPlan,
	publicationByRecord map[checkpointmodel.RecordID]PinnedPublication,
) (DiscardResult, error) {
	var removed uint64
	for _, action := range plan.Actions() {
		if err := ctx.Err(); err != nil {
			return DiscardResult{}, err
		}
		publication := publicationByRecord[action.RecordID()]
		if publication == nil {
			return DiscardResult{}, ErrInvalidContract
		}
		if attention, err := revalidatePublication(ctx, publication); err != nil {
			return DiscardResult{}, err
		} else if attention.Valid() {
			return NewDiscardResult(DiscardNeedsAttention, removed, []Attention{attention})
		}

		applied, err := leased.Apply(ctx, action)
		if err != nil {
			return DiscardResult{}, err
		}
		if applied.Status() == ApplyNeedsAttention {
			return NewDiscardResult(DiscardNeedsAttention, removed, applied.Attention())
		}
		if applied.Status() == ApplyCompleted && removalAction(action.Kind()) {
			removed++
		}

		if attention, err := revalidatePublication(ctx, publication); err != nil {
			return DiscardResult{}, err
		} else if attention.Valid() {
			return NewDiscardResult(DiscardNeedsAttention, removed, []Attention{attention})
		}
	}
	if removed == 0 {
		return NewDiscardResult(AlreadyAbsent, 0, nil)
	}
	return NewDiscardResult(Discarded, removed, nil)
}

func revalidatePublication(
	ctx context.Context,
	publication PinnedPublication,
) (Attention, error) {
	observation := publication.Observation()
	evidence, err := publication.Revalidate(ctx)
	if err != nil {
		return Attention{}, err
	}
	if evidence == observation.FinalEvidence() {
		return Attention{}, nil
	}
	reason := AttentionAmbiguousPublication
	if observation.FinalEvidence() == EvidenceExact && evidence == EvidenceReplaced {
		reason = AttentionReplacement
	}
	return derivedAttention(reason, observation.RecordID().Bytes()), nil
}

func discardPlanSettlement(plan DiscardPlan, removed uint64) (DiscardResult, error) {
	if !plan.Valid() {
		return DiscardResult{}, ErrInvalidContract
	}
	switch plan.ExpectedStatus() {
	case AlreadyAbsent:
		return NewDiscardResult(AlreadyAbsent, 0, nil)
	case DiscardNeedsAttention:
		return NewDiscardResult(DiscardNeedsAttention, removed, plan.Attention())
	default:
		return DiscardResult{}, ErrInvalidContract
	}
}

func removalAction(kind ActionKind) bool {
	return kind == ActionRemoveStage || kind == ActionRemoveAnchor || kind == ActionRemoveRecord
}
