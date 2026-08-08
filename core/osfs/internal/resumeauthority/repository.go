package resumeauthority

import (
	"context"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

// Evidence is a normalized no-follow identity observation. Exact means the
// adapter retained and revalidated the expected object; it never means merely
// that an entry with the expected name exists.
type Evidence uint8

const (
	EvidenceAbsent Evidence = iota + 1
	EvidenceExact
	EvidenceReplaced
	EvidenceAmbiguous
)

func (evidence Evidence) Valid() bool {
	return evidence >= EvidenceAbsent && evidence <= EvidenceAmbiguous
}

// CheckpointObservation contains only canonical model state and normalized
// identity evidence. Repository layout and platform handles remain private to
// the lease implementation.
type CheckpointObservation struct {
	recordID       checkpointmodel.RecordID
	record         checkpointmodel.Record
	recordEvidence Evidence
	stageEvidence  Evidence
	anchorEvidence Evidence
}

func NewCheckpointObservation(
	recordID checkpointmodel.RecordID,
	record checkpointmodel.Record,
	recordEvidence Evidence,
	stageEvidence Evidence,
	anchorEvidence Evidence,
) (CheckpointObservation, error) {
	observation := CheckpointObservation{
		recordID:       recordID,
		record:         record,
		recordEvidence: recordEvidence,
		stageEvidence:  stageEvidence,
		anchorEvidence: anchorEvidence,
	}
	if !observation.validShape() {
		return CheckpointObservation{}, ErrInvalidContract
	}
	return observation, nil
}

func (observation CheckpointObservation) RecordID() checkpointmodel.RecordID {
	return observation.recordID
}

func (observation CheckpointObservation) Record() checkpointmodel.Record {
	return observation.record
}

func (observation CheckpointObservation) RecordEvidence() Evidence {
	return observation.recordEvidence
}

func (observation CheckpointObservation) StageEvidence() Evidence {
	return observation.stageEvidence
}

func (observation CheckpointObservation) AnchorEvidence() Evidence {
	return observation.anchorEvidence
}

func (observation CheckpointObservation) validShape() bool {
	if observation.recordID.IsZero() || !observation.recordEvidence.Valid() ||
		!observation.stageEvidence.Valid() || !observation.anchorEvidence.Valid() {
		return false
	}
	if observation.recordEvidence == EvidenceExact {
		return observation.record.Valid() && observation.record.RecordID() == observation.recordID
	}
	return observation.record.RecordID().IsZero()
}

// PublicationObservation is supplied by the guarded output-path authority,
// separately from the private checkpoint repository. Exact is required before
// witnesses of a published record may be removed.
type PublicationObservation struct {
	recordID checkpointmodel.RecordID
	final    Evidence
}

func NewPublicationObservation(
	recordID checkpointmodel.RecordID,
	final Evidence,
) (PublicationObservation, error) {
	if recordID.IsZero() || !final.Valid() {
		return PublicationObservation{}, ErrInvalidContract
	}
	return PublicationObservation{recordID: recordID, final: final}, nil
}

func (observation PublicationObservation) RecordID() checkpointmodel.RecordID {
	return observation.recordID
}

func (observation PublicationObservation) FinalEvidence() Evidence {
	return observation.final
}

// RepositorySnapshot is taken only after acquiring the selected intent lease.
// NamespaceEvidenceExact means the root, backend, intent, inventory pin, and
// lease carrier were all revalidated before any action is considered.
type RepositorySnapshot struct {
	namespaceEvidence Evidence
	binding           checkpointmodel.Binding
	checkpoints       []CheckpointObservation
	attention         []Attention
}

func NewRepositorySnapshot(
	namespaceEvidence Evidence,
	binding checkpointmodel.Binding,
	checkpoints []CheckpointObservation,
	attention []Attention,
) (RepositorySnapshot, error) {
	snapshot := RepositorySnapshot{
		namespaceEvidence: namespaceEvidence,
		binding:           binding,
		checkpoints:       slices.Clone(checkpoints),
		attention:         slices.Clone(attention),
	}
	if !snapshot.validShape() {
		return RepositorySnapshot{}, ErrInvalidContract
	}
	return snapshot, nil
}

func (snapshot RepositorySnapshot) NamespaceEvidence() Evidence {
	return snapshot.namespaceEvidence
}

func (snapshot RepositorySnapshot) Binding() checkpointmodel.Binding {
	return snapshot.binding
}

func (snapshot RepositorySnapshot) Checkpoints() []CheckpointObservation {
	return slices.Clone(snapshot.checkpoints)
}

func (snapshot RepositorySnapshot) Attention() []Attention {
	return slices.Clone(snapshot.attention)
}

func (snapshot RepositorySnapshot) validShape() bool {
	if !snapshot.namespaceEvidence.Valid() {
		return false
	}
	if snapshot.namespaceEvidence == EvidenceExact && !snapshot.binding.Valid() {
		return false
	}
	for _, checkpoint := range snapshot.checkpoints {
		if !checkpoint.validShape() {
			return false
		}
	}
	for _, attention := range snapshot.attention {
		if !attention.Valid() {
			return false
		}
	}
	return true
}

type ActionKind uint8

const (
	ActionRemoveStage ActionKind = iota + 1
	ActionSyncStages
	ActionRemoveAnchor
	ActionSyncAnchors
	ActionRemoveRecord
	ActionSyncRecords
)

func (kind ActionKind) Valid() bool {
	return kind >= ActionRemoveStage && kind <= ActionSyncRecords
}

// Action is intentionally constructible only by ReduceDiscard. The adapter
// must still revalidate every pin immediately before applying it; a plan is
// policy authority, never a substitute for current native identity proof.
type Action struct {
	kind   ActionKind
	record checkpointmodel.Record
}

func (action Action) Kind() ActionKind                   { return action.kind }
func (action Action) Record() checkpointmodel.Record     { return action.record }
func (action Action) RecordID() checkpointmodel.RecordID { return action.record.RecordID() }
func (action Action) ObjectID() checkpointmodel.ObjectID { return action.record.OwnedOutputObject() }

func (action Action) Valid() bool {
	return action.kind.Valid() && action.record.Valid() &&
		checkpointmodel.Committed(action.record)
}

type ApplyStatus uint8

const (
	ApplyCompleted ApplyStatus = iota + 1
	ApplyAlreadySatisfied
	ApplyNeedsAttention
)

func (status ApplyStatus) Valid() bool {
	return status >= ApplyCompleted && status <= ApplyNeedsAttention
}

type ApplyResult struct {
	status    ApplyStatus
	attention []Attention
}

func NewApplyResult(status ApplyStatus, attention []Attention) (ApplyResult, error) {
	result := ApplyResult{status: status, attention: slices.Clone(attention)}
	if !result.valid() {
		return ApplyResult{}, ErrInvalidContract
	}
	return result, nil
}

func (result ApplyResult) Status() ApplyStatus    { return result.status }
func (result ApplyResult) Attention() []Attention { return slices.Clone(result.attention) }

func (result ApplyResult) valid() bool {
	if !result.status.Valid() {
		return false
	}
	for _, attention := range result.attention {
		if !attention.Valid() {
			return false
		}
	}
	if result.status == ApplyNeedsAttention {
		return len(result.attention) > 0
	}
	return len(result.attention) == 0
}

// LeasedRepository is the mutation half of the consumer-side port. Apply must
// compare the exact pinned object again and synchronize the corresponding
// parent for sync actions. Busy is returned by PinnedInventory.Acquire before
// this capability exists, so contention has no mutation authority.
type LeasedRepository interface {
	Observe(context.Context) (RepositorySnapshot, error)
	Apply(context.Context, Action) (ApplyResult, error)
	Close() error
}
