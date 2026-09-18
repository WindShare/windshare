package recovery

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/task"
)

var ErrContract = errors.New("recovery authority violated its contract")

type OperationState uint8

const (
	OperationIncomplete OperationState = iota + 1
	OperationResumable
	OperationCleanupPending
	OperationNeedsAttention
)

func (state OperationState) Valid() bool {
	return state >= OperationIncomplete && state <= OperationNeedsAttention
}

func (state OperationState) String() string {
	switch state {
	case OperationIncomplete:
		return "incomplete"
	case OperationResumable:
		return "resumable"
	case OperationCleanupPending:
		return "cleanup-pending"
	case OperationNeedsAttention:
		return "operation-needs-attention"
	default:
		return ""
	}
}

type BlockedReason uint8

const (
	BlockedPublicationUnknown BlockedReason = iota + 1
	BlockedCheckpointInvalid
	BlockedOwnedObjectUnknown
	BlockedRevisionConflict
)

func (reason BlockedReason) Valid() bool {
	return reason >= BlockedPublicationUnknown && reason <= BlockedRevisionConflict
}

func (reason BlockedReason) String() string {
	switch reason {
	case BlockedPublicationUnknown:
		return "publication-unknown"
	case BlockedCheckpointInvalid:
		return "checkpoint-invalid"
	case BlockedOwnedObjectUnknown:
		return "owned-object-unknown"
	case BlockedRevisionConflict:
		return "revision-conflict"
	default:
		return ""
	}
}

type AttentionReason string

const (
	AttentionNone        AttentionReason = ""
	AttentionDestination AttentionReason = "destination-ownership-unknown"
	AttentionRegistry    AttentionReason = "registry-ownership-unknown"
	AttentionLease       AttentionReason = "lease-ownership-unknown"
	AttentionOperation   AttentionReason = "operation-ownership-unknown"
	AttentionCleanup     AttentionReason = "cleanup-uncertain"
)

func (reason AttentionReason) Valid() bool {
	switch reason {
	case AttentionDestination, AttentionRegistry, AttentionLease, AttentionOperation, AttentionCleanup:
		return true
	default:
		return false
	}
}

type BlockedItem struct {
	ArtifactPath string
	PathKnown    bool
	Reason       BlockedReason
}

func (item BlockedItem) Valid() bool {
	if !item.Reason.Valid() {
		return false
	}
	if !item.PathKnown {
		return item.ArtifactPath == "" && item.Reason == BlockedCheckpointInvalid
	}
	canonical, err := catalog.CanonicalPath(item.ArtifactPath)
	return err == nil && canonical == item.ArtifactPath && item.ArtifactPath != ""
}

type Operation struct {
	ID           receivecontract.OperationID
	State        OperationState
	Attention    AttentionReason
	Running      bool
	BlockedItems []BlockedItem
}

func (operation Operation) Valid() bool {
	if operation.ID.IsZero() || !operation.State.Valid() ||
		!validAttention(operation.State, operation.Attention) {
		return false
	}
	if operation.Running && len(operation.BlockedItems) != 0 {
		return false
	}
	for _, item := range operation.BlockedItems {
		if !item.Valid() {
			return false
		}
	}
	return true
}

func validAttention(state OperationState, reason AttentionReason) bool {
	switch state {
	case OperationNeedsAttention:
		return reason.Valid() && reason != AttentionCleanup
	case OperationCleanupPending:
		return reason == AttentionNone || reason == AttentionCleanup
	default:
		return reason == AttentionNone
	}
}

type Snapshot struct {
	Operations      []Operation
	RegistryUnknown bool
}

func NewSnapshot(operations []Operation, registryUnknown bool) (Snapshot, error) {
	snapshot := Snapshot{Operations: operations, RegistryUnknown: registryUnknown}.Clone()
	slices.SortFunc(snapshot.Operations, func(left, right Operation) int {
		return slices.Compare(left.ID.Bytes(), right.ID.Bytes())
	})
	if !snapshot.Valid() {
		return Snapshot{}, ErrContract
	}
	return snapshot, nil
}

func (snapshot Snapshot) Clone() Snapshot {
	cloned := Snapshot{
		Operations: slices.Clone(snapshot.Operations), RegistryUnknown: snapshot.RegistryUnknown,
	}
	for index := range cloned.Operations {
		cloned.Operations[index].BlockedItems = slices.Clone(cloned.Operations[index].BlockedItems)
	}
	return cloned
}

func (snapshot Snapshot) Valid() bool {
	for index, operation := range snapshot.Operations {
		if !operation.Valid() || index > 0 &&
			slices.Compare(snapshot.Operations[index-1].ID.Bytes(), operation.ID.Bytes()) >= 0 {
			return false
		}
	}
	return true
}

func (snapshot Snapshot) NeedsAttention() bool {
	if snapshot.RegistryUnknown {
		return true
	}
	for _, operation := range snapshot.Operations {
		if operation.State == OperationNeedsAttention {
			return true
		}
	}
	return false
}

type DiscardStatus string

const (
	Discarded             DiscardStatus = "discarded"
	DiscardCleanupPending DiscardStatus = "cleanup-pending"
	DiscardNeedsAttention DiscardStatus = "operation-needs-attention"
)

type DiscardReport struct {
	Status       DiscardStatus
	ID           receivecontract.OperationID
	Attention    AttentionReason
	BlockedItems []BlockedItem
}

func (report DiscardReport) Valid() bool {
	if report.ID.IsZero() {
		return false
	}
	for _, item := range report.BlockedItems {
		if !item.Valid() {
			return false
		}
	}
	switch report.Status {
	case Discarded:
		return report.Attention == AttentionNone
	case DiscardCleanupPending:
		return report.Attention == AttentionNone || report.Attention == AttentionCleanup
	case DiscardNeedsAttention:
		return report.Attention.Valid() && report.Attention != AttentionCleanup
	default:
		return false
	}
}

type DiscardResult struct {
	Report       DiscardReport
	Failure      *Failure
	Err          error
	CleanupError error
	// The facade supplies a completed, bounded stream after native leases join.
	Observations <-chan task.Observation
}

func (result DiscardResult) Successful() bool {
	return result.Report.Valid() && result.Report.Status == Discarded && result.Err == nil &&
		result.CleanupError == nil && result.Failure == nil
}
