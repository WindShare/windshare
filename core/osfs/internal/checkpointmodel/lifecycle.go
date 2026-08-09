package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
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

func EncodeReceiveLifecycleState(record ReceiveLifecycleState) ([]byte, error) {
	if !record.Valid() {
		return nil, ErrInvalidLifecycleState
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(ReceiveLifecycleDomain)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(ReceiveLifecycleVersion)
	writeLifecycleFrame(&encoded, record.operationID.Bytes())
	writeLifecycleFrame(&encoded, record.receiveIntent.Bytes())
	writeLifecycleUint64(&encoded, record.stateGeneration)
	_ = encoded.WriteByte(byte(record.phase))
	writeLifecycleUint64(&encoded, record.expiresAtMillis)
	writeLifecycleUint64(&encoded, record.successCount)
	writeLifecycleUint64(&encoded, record.failureCount)
	_ = encoded.WriteByte(byte(record.partialReason))
	_ = encoded.WriteByte(byte(record.attentionReason))
	_ = encoded.WriteByte(byte(record.cleanupState))
	_ = encoded.WriteByte(byte(record.priorStableState))
	_, _ = encoded.Write(record.receiptDigest[:])
	writeLifecycleUint32(&encoded, uint32(len(record.checkpointRefs)))
	for _, reference := range record.checkpointRefs {
		_, _ = encoded.Write(reference.recordID.Bytes())
		writeLifecycleUint64(&encoded, reference.generation)
	}
	return encoded.Bytes(), nil
}

func DecodeReceiveLifecycleState(encoded []byte) (ReceiveLifecycleState, error) {
	if len(encoded) == 0 || len(encoded) > maximumLifecycleRecordBytes {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	prefix := append(append([]byte(nil), ReceiveLifecycleDomain...), 0, ReceiveLifecycleVersion)
	if !bytes.HasPrefix(encoded, prefix) {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	cursor := lifecycleCursor{raw: encoded, offset: len(prefix)}
	operationRaw, err := cursor.frame(receivecontract.StableIdentityBytes)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	operation, err := receivecontract.OperationIDFromBytes(operationRaw)
	if err != nil {
		return ReceiveLifecycleState{}, errors.Join(ErrInvalidLifecycleState, err)
	}
	intentRaw, err := cursor.frame(transfer.ReceiveIntentDigestBytes)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	intent, err := transfer.ReceiveIntentDigestFromBytes(intentRaw)
	if err != nil {
		return ReceiveLifecycleState{}, errors.Join(ErrInvalidLifecycleState, err)
	}
	generation, err := cursor.uint64()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	phase, err := cursor.byte()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	expires, err := cursor.uint64()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	successes, err := cursor.uint64()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	failures, err := cursor.uint64()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	partial, err := cursor.byte()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	attention, err := cursor.byte()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	cleanup, err := cursor.byte()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	prior, err := cursor.byte()
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	receiptRaw, err := cursor.take(sha256.Size)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	var receipt AggregateDigest
	copy(receipt[:], receiptRaw)
	count, err := cursor.uint32()
	if err != nil || count > MaximumAggregateReferences {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	references := make([]FileCheckpointReference, count)
	for index := range references {
		recordRaw, takeErr := cursor.take(sha256.Size)
		if takeErr != nil {
			return ReceiveLifecycleState{}, takeErr
		}
		recordID, parseErr := RecordIDFromBytes(recordRaw)
		if parseErr != nil {
			return ReceiveLifecycleState{}, errors.Join(ErrInvalidLifecycleState, parseErr)
		}
		checkpointGeneration, takeErr := cursor.uint64()
		if takeErr != nil {
			return ReceiveLifecycleState{}, takeErr
		}
		references[index], parseErr = FileCheckpointReferenceFromIdentity(recordID, checkpointGeneration)
		if parseErr != nil {
			return ReceiveLifecycleState{}, parseErr
		}
	}
	if cursor.offset != len(encoded) {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	record, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: operation, ReceiveIntent: intent, StateGeneration: generation,
		Phase: LifecyclePhase(phase), CheckpointRefs: references, ReceiptDigest: receipt,
		ExpiresAtMillis: expires, SuccessCount: successes, FailureCount: failures,
		PartialReason: PartialDirectoryReason(partial), AttentionReason: NeedsAttentionReason(attention),
		CleanupState: OwnedCleanupState(cleanup), PriorStableState: LifecyclePhase(prior),
	})
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	canonical, _ := EncodeReceiveLifecycleState(record)
	if !bytes.Equal(canonical, encoded) {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	return record, nil
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

func (receipt DirectTreeReceipt) CanonicalBytes() []byte {
	if !receipt.Valid() {
		return nil
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(ReceiveReceiptDomain)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(ReceiveReceiptVersion)
	_ = encoded.WriteByte(byte(receipt.kind))
	writeLifecycleFrame(&encoded, receipt.operationID.Bytes())
	writeLifecycleFrame(&encoded, receipt.receiveIntent.Bytes())
	writeLifecycleFrame(&encoded, receipt.reservationDigest.Bytes())
	_, _ = encoded.Write(receipt.evidenceDigest[:])
	writeLifecycleUint64(&encoded, receipt.successCount)
	writeLifecycleUint64(&encoded, receipt.failureCount)
	_ = encoded.WriteByte(byte(receipt.partialReason))
	writeLifecycleUint64(&encoded, receipt.cleanupGeneration)
	writeLifecycleUint64(&encoded, receipt.removedObjectCount)
	writeLifecycleUint64(&encoded, receipt.removedRecordCount)
	writeLifecycleUint32(&encoded, uint32(len(receipt.checkpointRefs)))
	for _, reference := range receipt.checkpointRefs {
		_, _ = encoded.Write(reference.recordID.Bytes())
		writeLifecycleUint64(&encoded, reference.generation)
	}
	return encoded.Bytes()
}

func (receipt DirectTreeReceipt) Digest() AggregateDigest {
	if !receipt.Valid() {
		return AggregateDigest{}
	}
	return AggregateDigest(sha256.Sum256(receipt.CanonicalBytes()))
}

func DecodeDirectTreeReceipt(encoded []byte) (DirectTreeReceipt, error) {
	if len(encoded) == 0 || len(encoded) > maximumLifecycleRecordBytes {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	prefix := append(append([]byte(nil), ReceiveReceiptDomain...), 0, ReceiveReceiptVersion)
	if !bytes.HasPrefix(encoded, prefix) {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	cursor := lifecycleCursor{raw: encoded, offset: len(prefix)}
	kind, err := cursor.byte()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	operationRaw, err := cursor.frame(receivecontract.StableIdentityBytes)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	operation, err := receivecontract.OperationIDFromBytes(operationRaw)
	if err != nil {
		return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, err)
	}
	intentRaw, err := cursor.frame(transfer.ReceiveIntentDigestBytes)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	intent, err := transfer.ReceiveIntentDigestFromBytes(intentRaw)
	if err != nil {
		return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, err)
	}
	bindingRaw, err := cursor.frame(sha256.Size)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	binding, err := receivecontract.BindingDigestFromBytes(bindingRaw)
	if err != nil {
		return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, err)
	}
	evidenceRaw, err := cursor.take(sha256.Size)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	evidence, err := AggregateDigestFromBytes(evidenceRaw)
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	successes, err := cursor.uint64()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	failures, err := cursor.uint64()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	partial, err := cursor.byte()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	cleanupGeneration, err := cursor.uint64()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	removedObjects, err := cursor.uint64()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	removedRecords, err := cursor.uint64()
	if err != nil {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	count, err := cursor.uint32()
	if err != nil || count > MaximumAggregateReferences {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	references := make([]FileCheckpointReference, count)
	for index := range references {
		recordRaw, takeErr := cursor.take(sha256.Size)
		if takeErr != nil {
			return DirectTreeReceipt{}, ErrInvalidReceipt
		}
		recordID, parseErr := RecordIDFromBytes(recordRaw)
		if parseErr != nil {
			return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, parseErr)
		}
		generation, takeErr := cursor.uint64()
		if takeErr != nil {
			return DirectTreeReceipt{}, ErrInvalidReceipt
		}
		references[index], parseErr = FileCheckpointReferenceFromIdentity(recordID, generation)
		if parseErr != nil {
			return DirectTreeReceipt{}, ErrInvalidReceipt
		}
	}
	if cursor.offset != len(encoded) {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	receipt, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: DirectTreeReceiptKind(kind), OperationID: operation, ReceiveIntent: intent,
		ReservationDigest: binding, CheckpointRefs: references, EvidenceDigest: evidence,
		SuccessCount: successes, FailureCount: failures,
		PartialReason: PartialDirectoryReason(partial), CleanupGeneration: cleanupGeneration,
		RemovedObjectCount: removedObjects, RemovedRecordCount: removedRecords,
	})
	if err != nil || !bytes.Equal(receipt.CanonicalBytes(), encoded) {
		return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, err)
	}
	return receipt, nil
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

func writeLifecycleFrame(target *bytes.Buffer, value []byte) {
	writeLifecycleUint64(target, uint64(len(value)))
	_, _ = target.Write(value)
}

func writeLifecycleUint64(target *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func writeLifecycleUint32(target *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

type lifecycleCursor struct {
	raw    []byte
	offset int
}

func (cursor *lifecycleCursor) take(count int) ([]byte, error) {
	if count < 0 || cursor.offset < 0 || count > len(cursor.raw)-cursor.offset {
		return nil, ErrInvalidLifecycleState
	}
	value := cursor.raw[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return value, nil
}

func (cursor *lifecycleCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *lifecycleCursor) frame(maximum int) ([]byte, error) {
	length, err := cursor.uint64()
	if err != nil || length == 0 || length > uint64(maximum) || length > uint64(len(cursor.raw)-cursor.offset) {
		return nil, ErrInvalidLifecycleState
	}
	return cursor.take(int(length))
}

func (cursor *lifecycleCursor) uint32() (uint32, error) {
	value, err := cursor.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}

func (cursor *lifecycleCursor) uint64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func NextStableExpiry(nowMillis uint64) (uint64, error) {
	if nowMillis > math.MaxUint64-StableRetentionMilliseconds {
		return 0, ErrInvalidLifecycleState
	}
	return nowMillis + StableRetentionMilliseconds, nil
}
