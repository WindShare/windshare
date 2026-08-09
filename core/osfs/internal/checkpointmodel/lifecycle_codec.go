package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

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
	decoder, err := newAggregateDecoder(
		encoded, ReceiveLifecycleDomain, ReceiveLifecycleVersion, ErrInvalidLifecycleState,
	)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	spec := decoder.receiveLifecycleSpec()
	if err := decoder.finish(); err != nil {
		return ReceiveLifecycleState{}, err
	}
	record, err := NewReceiveLifecycleState(spec)
	if err != nil {
		return ReceiveLifecycleState{}, err
	}
	canonical, _ := EncodeReceiveLifecycleState(record)
	if !bytes.Equal(canonical, encoded) {
		return ReceiveLifecycleState{}, ErrInvalidLifecycleState
	}
	return record, nil
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
	decoder, err := newAggregateDecoder(
		encoded, ReceiveReceiptDomain, ReceiveReceiptVersion, ErrInvalidReceipt,
	)
	if err != nil {
		return DirectTreeReceipt{}, err
	}
	spec := decoder.directTreeReceiptSpec()
	if err := decoder.finish(); err != nil {
		return DirectTreeReceipt{}, err
	}
	receipt, err := NewDirectTreeReceipt(spec)
	if err != nil {
		return DirectTreeReceipt{}, errors.Join(ErrInvalidReceipt, err)
	}
	if !bytes.Equal(receipt.CanonicalBytes(), encoded) {
		return DirectTreeReceipt{}, ErrInvalidReceipt
	}
	return receipt, nil
}

// aggregateDecoder keeps framing, identity parsing, and failure-domain mapping
// together so model constructors only see a complete wire projection.
type aggregateDecoder struct {
	cursor  lifecycleCursor
	invalid error
	err     error
}

func newAggregateDecoder(
	encoded []byte,
	domain string,
	version uint8,
	invalid error,
) (aggregateDecoder, error) {
	if len(encoded) == 0 || len(encoded) > maximumLifecycleRecordBytes {
		return aggregateDecoder{}, invalid
	}
	prefix := make([]byte, 0, len(domain)+2)
	prefix = append(prefix, domain...)
	prefix = append(prefix, 0, version)
	if !bytes.HasPrefix(encoded, prefix) {
		return aggregateDecoder{}, invalid
	}
	return aggregateDecoder{
		cursor:  lifecycleCursor{raw: encoded, offset: len(prefix)},
		invalid: invalid,
	}, nil
}

func (decoder *aggregateDecoder) receiveLifecycleSpec() LifecycleStateSpec {
	var spec LifecycleStateSpec
	spec.OperationID = decoder.operationID()
	spec.ReceiveIntent = decoder.receiveIntentDigest()
	spec.StateGeneration = decoder.uint64()
	spec.Phase = LifecyclePhase(decoder.byte())
	spec.ExpiresAtMillis = decoder.uint64()
	spec.SuccessCount = decoder.uint64()
	spec.FailureCount = decoder.uint64()
	spec.PartialReason = PartialDirectoryReason(decoder.byte())
	spec.AttentionReason = NeedsAttentionReason(decoder.byte())
	spec.CleanupState = OwnedCleanupState(decoder.byte())
	spec.PriorStableState = LifecyclePhase(decoder.byte())
	spec.ReceiptDigest = decoder.aggregateDigest()
	spec.CheckpointRefs = decoder.checkpointReferences()
	return spec
}

func (decoder *aggregateDecoder) directTreeReceiptSpec() DirectTreeReceiptSpec {
	var spec DirectTreeReceiptSpec
	spec.Kind = DirectTreeReceiptKind(decoder.byte())
	spec.OperationID = decoder.operationID()
	spec.ReceiveIntent = decoder.receiveIntentDigest()
	spec.ReservationDigest = decoder.bindingDigest()
	spec.EvidenceDigest = decoder.aggregateDigest()
	spec.SuccessCount = decoder.uint64()
	spec.FailureCount = decoder.uint64()
	spec.PartialReason = PartialDirectoryReason(decoder.byte())
	spec.CleanupGeneration = decoder.uint64()
	spec.RemovedObjectCount = decoder.uint64()
	spec.RemovedRecordCount = decoder.uint64()
	spec.CheckpointRefs = decoder.checkpointReferences()
	return spec
}

func (decoder *aggregateDecoder) operationID() receivecontract.OperationID {
	raw := decoder.frame(receivecontract.StableIdentityBytes)
	if decoder.err != nil {
		return receivecontract.OperationID{}
	}
	value, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		decoder.err = errors.Join(decoder.invalid, err)
	}
	return value
}

func (decoder *aggregateDecoder) receiveIntentDigest() transfer.ReceiveIntentDigest {
	raw := decoder.frame(transfer.ReceiveIntentDigestBytes)
	if decoder.err != nil {
		return transfer.ReceiveIntentDigest{}
	}
	value, err := transfer.ReceiveIntentDigestFromBytes(raw)
	if err != nil {
		decoder.err = errors.Join(decoder.invalid, err)
	}
	return value
}

func (decoder *aggregateDecoder) bindingDigest() receivecontract.BindingDigest {
	raw := decoder.frame(sha256.Size)
	if decoder.err != nil {
		return receivecontract.BindingDigest{}
	}
	value, err := receivecontract.BindingDigestFromBytes(raw)
	if err != nil {
		decoder.err = errors.Join(decoder.invalid, err)
	}
	return value
}

func (decoder *aggregateDecoder) aggregateDigest() AggregateDigest {
	raw := decoder.take(sha256.Size)
	var digest AggregateDigest
	copy(digest[:], raw)
	return digest
}

func (decoder *aggregateDecoder) checkpointReferences() []FileCheckpointReference {
	count := decoder.uint32()
	if decoder.err != nil {
		return nil
	}
	if count > MaximumAggregateReferences {
		decoder.err = decoder.invalid
		return nil
	}
	references := make([]FileCheckpointReference, int(count))
	for index := range references {
		recordRaw := decoder.take(sha256.Size)
		if decoder.err != nil {
			return nil
		}
		recordID, err := RecordIDFromBytes(recordRaw)
		if err != nil {
			decoder.err = errors.Join(decoder.invalid, err)
			return nil
		}
		generation := decoder.uint64()
		if decoder.err != nil {
			return nil
		}
		reference, err := FileCheckpointReferenceFromIdentity(recordID, generation)
		if err != nil {
			decoder.err = decoder.invalid
			return nil
		}
		references[index] = reference
	}
	return references
}

func (decoder *aggregateDecoder) finish() error {
	if decoder.err != nil {
		return decoder.err
	}
	if decoder.cursor.offset != len(decoder.cursor.raw) {
		return decoder.invalid
	}
	return nil
}

func (decoder *aggregateDecoder) take(count int) []byte {
	if decoder.err != nil {
		return nil
	}
	value, err := decoder.cursor.take(count)
	if err != nil {
		decoder.err = decoder.invalid
		return nil
	}
	return value
}

func (decoder *aggregateDecoder) byte() byte {
	if decoder.err != nil {
		return 0
	}
	value, err := decoder.cursor.byte()
	if err != nil {
		decoder.err = decoder.invalid
		return 0
	}
	return value
}

func (decoder *aggregateDecoder) frame(maximum int) []byte {
	if decoder.err != nil {
		return nil
	}
	value, err := decoder.cursor.frame(maximum)
	if err != nil {
		decoder.err = decoder.invalid
		return nil
	}
	return value
}

func (decoder *aggregateDecoder) uint32() uint32 {
	if decoder.err != nil {
		return 0
	}
	value, err := decoder.cursor.uint32()
	if err != nil {
		decoder.err = decoder.invalid
		return 0
	}
	return value
}

func (decoder *aggregateDecoder) uint64() uint64 {
	if decoder.err != nil {
		return 0
	}
	value, err := decoder.cursor.uint64()
	if err != nil {
		decoder.err = decoder.invalid
		return 0
	}
	return value
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
