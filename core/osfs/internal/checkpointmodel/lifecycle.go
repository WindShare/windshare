package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"math"
	"slices"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	ReceiveLifecycleVersion     = uint8(1)
	ReceiveLifecycleDomain      = "windshare/receive-lifecycle-state/v1"
	ReceiveReceiptVersion       = uint8(1)
	ReceiveReceiptDomain        = "windshare/receive-receipt/v1"
	StableRetentionMilliseconds = uint64(86_400_000)
	MaximumAggregateReferences  = 1_048_576
	maximumLifecycleRecordBytes = 64 * 1024 * 1024
)

var (
	ErrInvalidLifecycleState = errors.New("receive lifecycle state is invalid")
	ErrInvalidReceipt        = errors.New("receive receipt is invalid")
)

type LifecyclePhase uint8

const (
	LifecycleIntentFrozen LifecyclePhase = iota + 1
	LifecyclePreparing
	LifecycleReceiving
	LifecycleResumableReceive
	LifecycleFinalizingTree
	LifecycleCommittingAtomic
	LifecycleMaterializationSealed
	LifecyclePackaging
	LifecycleResumablePackage
	LifecycleArtifactSealed
	LifecycleWaitingToSave
	LifecyclePublishingManaged
	LifecycleHandingOff
	LifecyclePublished
	LifecycleDownloadStarted
	LifecyclePartialDirectory
	LifecycleRestartRequired
	LifecycleDiscarded
	LifecycleExpired
	LifecycleNeedsAttention
)

func (phase LifecyclePhase) Valid() bool {
	return phase >= LifecycleIntentFrozen && phase <= LifecycleNeedsAttention
}

type NeedsAttentionReason uint8

const (
	AttentionTargetOwnershipUnknown NeedsAttentionReason = iota + 1
	AttentionPublicationUnknown
	AttentionCleanupUnknown
)

func (reason NeedsAttentionReason) Valid() bool {
	return reason >= AttentionTargetOwnershipUnknown && reason <= AttentionCleanupUnknown
}

func (reason NeedsAttentionReason) String() string {
	switch reason {
	case AttentionTargetOwnershipUnknown:
		return "target-ownership-unknown"
	case AttentionPublicationUnknown:
		return "publication-unknown"
	case AttentionCleanupUnknown:
		return "cleanup-unknown"
	default:
		return ""
	}
}

type PartialDirectoryReason uint8

const (
	PartialDirectoryFailures PartialDirectoryReason = iota + 1
	PartialDirectoryStopped
)

func (reason PartialDirectoryReason) Valid() bool {
	return reason == PartialDirectoryFailures || reason == PartialDirectoryStopped
}

type OwnedCleanupState uint8

const (
	OwnedCleanupClean OwnedCleanupState = iota + 1
	OwnedCleanupPending
)

func (state OwnedCleanupState) Valid() bool {
	return state == OwnedCleanupClean || state == OwnedCleanupPending
}

type AggregateDigest [sha256.Size]byte

func AggregateDigestFromBytes(raw []byte) (AggregateDigest, error) {
	if len(raw) != sha256.Size {
		return AggregateDigest{}, ErrInvalidLifecycleState
	}
	var digest AggregateDigest
	copy(digest[:], raw)
	if digest.IsZero() {
		return AggregateDigest{}, ErrInvalidLifecycleState
	}
	return digest, nil
}

func (digest AggregateDigest) Bytes() []byte { return slices.Clone(digest[:]) }
func (digest AggregateDigest) IsZero() bool  { return digest == AggregateDigest{} }

// FileCheckpointReference is deliberately narrower than FileCheckpointV2. It
// lets aggregate receipts prove which checkpoint cut they consumed without
// copying ranges into a second source of byte truth.
type FileCheckpointReference struct {
	recordID   RecordID
	generation uint64
}

func NewFileCheckpointReference(record Record) (FileCheckpointReference, error) {
	if !record.Valid() ||
		(record.CommitState() != CommitVerified && record.CommitState() != CommitPublished) {
		return FileCheckpointReference{}, ErrInvalidLifecycleState
	}
	return FileCheckpointReference{
		recordID: record.RecordID(), generation: record.CheckpointGeneration(),
	}, nil
}

func FileCheckpointReferenceFromIdentity(
	recordID RecordID,
	generation uint64,
) (FileCheckpointReference, error) {
	if recordID.IsZero() {
		return FileCheckpointReference{}, ErrInvalidLifecycleState
	}
	return FileCheckpointReference{recordID: recordID, generation: generation}, nil
}

func (reference FileCheckpointReference) RecordID() RecordID { return reference.recordID }
func (reference FileCheckpointReference) CheckpointGeneration() uint64 {
	return reference.generation
}
func (reference FileCheckpointReference) valid() bool {
	return !reference.recordID.IsZero()
}

type LifecycleStateSpec struct {
	OperationID      receivecontract.OperationID
	ReceiveIntent    transfer.ReceiveIntentDigest
	StateGeneration  uint64
	Phase            LifecyclePhase
	CheckpointRefs   []FileCheckpointReference
	ReceiptDigest    AggregateDigest
	ExpiresAtMillis  uint64
	SuccessCount     uint64
	FailureCount     uint64
	PartialReason    PartialDirectoryReason
	AttentionReason  NeedsAttentionReason
	CleanupState     OwnedCleanupState
	PriorStableState LifecyclePhase
}

type ReceiveLifecycleState struct {
	operationID      receivecontract.OperationID
	receiveIntent    transfer.ReceiveIntentDigest
	stateGeneration  uint64
	phase            LifecyclePhase
	checkpointRefs   []FileCheckpointReference
	receiptDigest    AggregateDigest
	expiresAtMillis  uint64
	successCount     uint64
	failureCount     uint64
	partialReason    PartialDirectoryReason
	attentionReason  NeedsAttentionReason
	cleanupState     OwnedCleanupState
	priorStableState LifecyclePhase
}

func NewReceiveLifecycleState(spec LifecycleStateSpec) (ReceiveLifecycleState, error) {
	references, err := canonicalCheckpointReferences(spec.CheckpointRefs)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	record := ReceiveLifecycleState{
		operationID: spec.OperationID, receiveIntent: spec.ReceiveIntent,
		stateGeneration: spec.StateGeneration, phase: spec.Phase,
		checkpointRefs: references, receiptDigest: spec.ReceiptDigest,
		expiresAtMillis: spec.ExpiresAtMillis, successCount: spec.SuccessCount,
		failureCount: spec.FailureCount, partialReason: spec.PartialReason,
		attentionReason: spec.AttentionReason, cleanupState: spec.CleanupState,
		priorStableState: spec.PriorStableState,
	}
	if err := record.validate(); err != nil {
		return ReceiveLifecycleState{}, err
	}
	return record, nil
}

func (record ReceiveLifecycleState) OperationID() receivecontract.OperationID {
	return record.operationID
}
func (record ReceiveLifecycleState) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return record.receiveIntent
}
func (record ReceiveLifecycleState) StateGeneration() uint64 { return record.stateGeneration }
func (record ReceiveLifecycleState) Phase() LifecyclePhase   { return record.phase }
func (record ReceiveLifecycleState) CheckpointReferences() []FileCheckpointReference {
	return slices.Clone(record.checkpointRefs)
}
func (record ReceiveLifecycleState) ReceiptDigest() AggregateDigest { return record.receiptDigest }
func (record ReceiveLifecycleState) ExpiresAtMillis() uint64        { return record.expiresAtMillis }
func (record ReceiveLifecycleState) SuccessCount() uint64           { return record.successCount }
func (record ReceiveLifecycleState) FailureCount() uint64           { return record.failureCount }
func (record ReceiveLifecycleState) PartialReason() PartialDirectoryReason {
	return record.partialReason
}
func (record ReceiveLifecycleState) AttentionReason() NeedsAttentionReason {
	return record.attentionReason
}
func (record ReceiveLifecycleState) CleanupState() OwnedCleanupState { return record.cleanupState }
func (record ReceiveLifecycleState) PriorStableState() LifecyclePhase {
	return record.priorStableState
}
func (record ReceiveLifecycleState) Valid() bool { return record.validate() == nil }

func (record ReceiveLifecycleState) validate() error {
	if record.operationID.IsZero() || record.receiveIntent.IsZero() ||
		record.stateGeneration == 0 || !record.phase.Valid() ||
		len(record.checkpointRefs) > MaximumAggregateReferences {
		return ErrInvalidLifecycleState
	}
	canonical, err := canonicalCheckpointReferences(record.checkpointRefs)
	if err != nil || !slices.Equal(canonical, record.checkpointRefs) {
		return ErrInvalidLifecycleState
	}
	switch record.phase {
	case LifecycleIntentFrozen:
		return record.requireEmptyAggregate()
	case LifecycleReceiving, LifecycleFinalizingTree:
		if record.expiresAtMillis != 0 || !record.receiptDigest.IsZero() ||
			record.partialReason != 0 || record.attentionReason != 0 ||
			record.cleanupState != 0 || record.priorStableState != 0 {
			return ErrInvalidLifecycleState
		}
	case LifecycleResumableReceive:
		if record.expiresAtMillis == 0 || !record.receiptDigest.IsZero() ||
			record.partialReason != 0 || record.attentionReason != 0 ||
			record.cleanupState != 0 || record.priorStableState != 0 {
			return ErrInvalidLifecycleState
		}
	case LifecyclePublished:
		if record.receiptDigest.IsZero() || record.expiresAtMillis != 0 ||
			record.failureCount != 0 || record.partialReason != 0 ||
			record.attentionReason != 0 || !record.cleanupState.Valid() ||
			record.priorStableState != 0 {
			return ErrInvalidLifecycleState
		}
	case LifecyclePartialDirectory:
		if record.receiptDigest.IsZero() || record.expiresAtMillis != 0 ||
			record.successCount == 0 || !record.partialReason.Valid() ||
			record.partialReason == PartialDirectoryFailures && record.failureCount == 0 ||
			record.attentionReason != 0 || record.cleanupState != 0 ||
			record.priorStableState != 0 {
			return ErrInvalidLifecycleState
		}
	case LifecycleDiscarded:
		if record.receiptDigest.IsZero() || record.expiresAtMillis != 0 ||
			record.successCount != 0 || record.failureCount != 0 ||
			record.partialReason != 0 || record.attentionReason != 0 ||
			record.cleanupState != OwnedCleanupClean || record.priorStableState != 0 ||
			len(record.checkpointRefs) != 0 {
			return ErrInvalidLifecycleState
		}
	case LifecycleExpired:
		if record.receiptDigest.IsZero() || record.expiresAtMillis == 0 ||
			record.partialReason != 0 || record.attentionReason != 0 ||
			!record.cleanupState.Valid() ||
			record.priorStableState != LifecycleResumableReceive {
			return ErrInvalidLifecycleState
		}
	case LifecycleNeedsAttention:
		if record.expiresAtMillis != 0 || !record.attentionReason.Valid() ||
			record.partialReason != 0 || record.cleanupState != 0 ||
			record.priorStableState != 0 {
			return ErrInvalidLifecycleState
		}
	default:
		// Core native persistence owns DirectTree only; accepting workspace or
		// portable variants here would invent recovery authority for another adapter.
		return ErrInvalidLifecycleState
	}
	return nil
}

func (record ReceiveLifecycleState) requireEmptyAggregate() error {
	if len(record.checkpointRefs) != 0 || !record.receiptDigest.IsZero() ||
		record.expiresAtMillis != 0 || record.successCount != 0 || record.failureCount != 0 ||
		record.partialReason != 0 || record.attentionReason != 0 ||
		record.cleanupState != 0 || record.priorStableState != 0 {
		return ErrInvalidLifecycleState
	}
	return nil
}

func ValidateLifecycleTransition(previous, next ReceiveLifecycleState) error {
	if !previous.Valid() || !next.Valid() ||
		previous.operationID != next.operationID || previous.receiveIntent != next.receiveIntent ||
		previous.stateGeneration == math.MaxUint64 ||
		next.stateGeneration != previous.stateGeneration+1 {
		return ErrInvalidLifecycleState
	}
	allowed := false
	switch previous.phase {
	case LifecycleIntentFrozen:
		allowed = next.phase == LifecycleReceiving || next.phase == LifecycleDiscarded ||
			next.phase == LifecycleNeedsAttention
	case LifecycleReceiving:
		allowed = next.phase == LifecycleResumableReceive || next.phase == LifecycleFinalizingTree ||
			next.phase == LifecyclePartialDirectory || next.phase == LifecycleDiscarded ||
			next.phase == LifecycleNeedsAttention
	case LifecycleResumableReceive:
		allowed = next.phase == LifecycleReceiving || next.phase == LifecycleFinalizingTree ||
			next.phase == LifecyclePartialDirectory || next.phase == LifecycleDiscarded ||
			next.phase == LifecycleExpired || next.phase == LifecycleNeedsAttention
	case LifecycleFinalizingTree:
		allowed = next.phase == LifecyclePublished || next.phase == LifecycleResumableReceive ||
			next.phase == LifecyclePartialDirectory || next.phase == LifecycleDiscarded ||
			next.phase == LifecycleNeedsAttention
	case LifecyclePublished:
		allowed = next.phase == LifecyclePublished &&
			previous.cleanupState == OwnedCleanupPending && next.cleanupState == OwnedCleanupClean ||
			next.phase == LifecycleNeedsAttention && next.attentionReason == AttentionCleanupUnknown
	case LifecycleExpired:
		allowed = next.phase == LifecycleExpired &&
			previous.cleanupState == OwnedCleanupPending && next.cleanupState == OwnedCleanupClean ||
			next.phase == LifecycleNeedsAttention &&
				(next.attentionReason == AttentionTargetOwnershipUnknown ||
					next.attentionReason == AttentionCleanupUnknown)
	}
	if !allowed {
		return ErrInvalidLifecycleState
	}
	return nil
}

type DirectTreeReceiptKind uint8

const (
	ReceiptTreeCompletion DirectTreeReceiptKind = iota + 1
	ReceiptPartialDirectory
	ReceiptCleanup
	ReceiptExpiry
)

type DirectTreeReceiptSpec struct {
	Kind               DirectTreeReceiptKind
	OperationID        receivecontract.OperationID
	ReceiveIntent      transfer.ReceiveIntentDigest
	ReservationDigest  receivecontract.BindingDigest
	CheckpointRefs     []FileCheckpointReference
	EvidenceDigest     AggregateDigest
	SuccessCount       uint64
	FailureCount       uint64
	PartialReason      PartialDirectoryReason
	CleanupGeneration  uint64
	RemovedObjectCount uint64
	RemovedRecordCount uint64
}

type DirectTreeReceipt struct {
	kind               DirectTreeReceiptKind
	operationID        receivecontract.OperationID
	receiveIntent      transfer.ReceiveIntentDigest
	reservationDigest  receivecontract.BindingDigest
	checkpointRefs     []FileCheckpointReference
	evidenceDigest     AggregateDigest
	successCount       uint64
	failureCount       uint64
	partialReason      PartialDirectoryReason
	cleanupGeneration  uint64
	removedObjectCount uint64
	removedRecordCount uint64
}

func NewDirectTreeReceipt(spec DirectTreeReceiptSpec) (DirectTreeReceipt, error) {
	references, err := canonicalCheckpointReferences(spec.CheckpointRefs)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	receipt := DirectTreeReceipt{
		kind: spec.Kind, operationID: spec.OperationID, receiveIntent: spec.ReceiveIntent,
		reservationDigest: spec.ReservationDigest, checkpointRefs: references,
		evidenceDigest: spec.EvidenceDigest, successCount: spec.SuccessCount,
		failureCount: spec.FailureCount, partialReason: spec.PartialReason,
		cleanupGeneration: spec.CleanupGeneration, removedObjectCount: spec.RemovedObjectCount,
		removedRecordCount: spec.RemovedRecordCount,
	}
	if !receipt.Valid() {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

func (receipt DirectTreeReceipt) Kind() DirectTreeReceiptKind { return receipt.kind }
func (receipt DirectTreeReceipt) OperationID() receivecontract.OperationID {
	return receipt.operationID
}
func (receipt DirectTreeReceipt) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return receipt.receiveIntent
}
func (receipt DirectTreeReceipt) ReservationDigest() receivecontract.BindingDigest {
	return receipt.reservationDigest
}
func (receipt DirectTreeReceipt) CheckpointReferences() []FileCheckpointReference {
	return slices.Clone(receipt.checkpointRefs)
}
func (receipt DirectTreeReceipt) EvidenceDigest() AggregateDigest { return receipt.evidenceDigest }
func (receipt DirectTreeReceipt) SuccessCount() uint64            { return receipt.successCount }
func (receipt DirectTreeReceipt) FailureCount() uint64            { return receipt.failureCount }
func (receipt DirectTreeReceipt) PartialReason() PartialDirectoryReason {
	return receipt.partialReason
}
func (receipt DirectTreeReceipt) CleanupGeneration() uint64  { return receipt.cleanupGeneration }
func (receipt DirectTreeReceipt) RemovedObjectCount() uint64 { return receipt.removedObjectCount }
func (receipt DirectTreeReceipt) RemovedRecordCount() uint64 { return receipt.removedRecordCount }

func (receipt DirectTreeReceipt) Valid() bool {
	if receipt.operationID.IsZero() || receipt.receiveIntent.IsZero() ||
		receipt.reservationDigest.IsZero() || receipt.evidenceDigest.IsZero() ||
		len(receipt.checkpointRefs) > MaximumAggregateReferences {
		return false
	}
	canonical, err := canonicalCheckpointReferences(receipt.checkpointRefs)
	if err != nil || !slices.Equal(canonical, receipt.checkpointRefs) {
		return false
	}
	switch receipt.kind {
	case ReceiptTreeCompletion:
		return receipt.failureCount == 0 && receipt.partialReason == 0 &&
			receipt.cleanupGeneration == 0 && receipt.removedObjectCount == 0 &&
			receipt.removedRecordCount == 0
	case ReceiptPartialDirectory:
		return receipt.successCount > 0 && receipt.partialReason.Valid() &&
			(receipt.partialReason != PartialDirectoryFailures || receipt.failureCount > 0) &&
			receipt.cleanupGeneration == 0 && receipt.removedObjectCount == 0 &&
			receipt.removedRecordCount == 0
	case ReceiptCleanup:
		return len(receipt.checkpointRefs) == 0 && receipt.successCount == 0 &&
			receipt.failureCount == 0 && receipt.partialReason == 0 &&
			receipt.cleanupGeneration > 0
	case ReceiptExpiry:
		return receipt.partialReason == 0 && receipt.cleanupGeneration > 0
	default:
		return false
	}
}

func canonicalCheckpointReferences(
	references []FileCheckpointReference,
) ([]FileCheckpointReference, error) {
	if len(references) > MaximumAggregateReferences {
		return nil, ErrInvalidLifecycleState
	}
	canonical := slices.Clone(references)
	for _, reference := range canonical {
		if !reference.valid() {
			return nil, ErrInvalidLifecycleState
		}
	}
	slices.SortFunc(canonical, func(left, right FileCheckpointReference) int {
		if compared := bytes.Compare(left.recordID[:], right.recordID[:]); compared != 0 {
			return compared
		}
		if left.generation < right.generation {
			return -1
		}
		if left.generation > right.generation {
			return 1
		}
		return 0
	})
	for index := 1; index < len(canonical); index++ {
		if canonical[index-1].recordID == canonical[index].recordID {
			// One aggregate must name exactly one authoritative generation per file checkpoint.
			return nil, ErrInvalidLifecycleState
		}
	}
	return canonical, nil
}

func NextStableExpiry(nowMillis uint64) (uint64, error) {
	if nowMillis > math.MaxUint64-StableRetentionMilliseconds {
		return 0, ErrInvalidLifecycleState
	}
	return nowMillis + StableRetentionMilliseconds, nil
}
