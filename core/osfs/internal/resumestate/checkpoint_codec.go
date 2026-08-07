package resumestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func DecodeFileCheckpointV1(encoded []byte) (FileCheckpointV1, error) {
	minimum := len(fileCheckpointMagic) + 4 + sha256.Size + 1
	if len(encoded) < minimum || !bytes.Equal(encoded[:len(fileCheckpointMagic)], []byte(fileCheckpointMagic)) {
		return FileCheckpointV1{}, fmt.Errorf("%w: envelope", ErrInvalidFileCheckpoint)
	}
	payloadEnd := len(encoded) - sha256.Size
	declared := binary.BigEndian.Uint32(encoded[len(fileCheckpointMagic) : len(fileCheckpointMagic)+4])
	actual := payloadEnd - len(fileCheckpointMagic) - 4
	if uint64(declared) != uint64(actual) {
		return FileCheckpointV1{}, fmt.Errorf("%w: payload length", ErrInvalidFileCheckpoint)
	}
	payload := encoded[len(fileCheckpointMagic)+4 : payloadEnd]
	var supplied FileCheckpointChecksum
	copy(supplied[:], encoded[payloadEnd:])
	hash := sha256.New()
	_, _ = hash.Write([]byte(fileCheckpointChecksumDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
	var expected FileCheckpointChecksum
	copy(expected[:], hash.Sum(nil))
	if supplied != expected {
		return FileCheckpointV1{}, ErrFileCheckpointChecksum
	}
	checkpoint, err := decodeCheckpointPayload(payload)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	checkpoint.checksum = supplied
	if err := checkpoint.valid(); err != nil {
		return FileCheckpointV1{}, err
	}
	if !bytes.Equal(checkpoint.canonicalPayload(), payload) {
		return FileCheckpointV1{}, ErrFileCheckpointNonCanonical
	}
	return checkpoint, nil
}

func DecodeFileCheckpointRecord(encoded []byte) (FileCheckpointRecord, error) {
	return DecodeFileCheckpointV1(encoded)
}

func ReadFileCheckpointV1(reader io.Reader) (FileCheckpointV1, error) {
	if reader == nil {
		return FileCheckpointV1{}, fmt.Errorf("%w: nil reader", ErrInvalidFileCheckpoint)
	}
	encoded, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return FileCheckpointV1{}, err
	}
	return DecodeFileCheckpointV1(encoded)
}

func WriteFileCheckpointV1(writer io.Writer, checkpoint FileCheckpointV1) error {
	if writer == nil {
		return fmt.Errorf("%w: nil writer", ErrInvalidFileCheckpoint)
	}
	encoded, err := EncodeFileCheckpointV1(checkpoint)
	if err != nil {
		return err
	}
	written, err := writer.Write(encoded)
	if err == nil && written != len(encoded) {
		return io.ErrShortWrite
	}
	return err
}

type checkpointCursor struct {
	bytes []byte
	off   int
}

func (cursor *checkpointCursor) take(count int) ([]byte, error) {
	if count < 0 || cursor.off < 0 || count > len(cursor.bytes)-cursor.off {
		return nil, fmt.Errorf("%w: truncated payload", ErrInvalidFileCheckpoint)
	}
	value := cursor.bytes[cursor.off : cursor.off+count]
	cursor.off += count
	return value, nil
}
func (cursor *checkpointCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}
func (cursor *checkpointCursor) u32() (uint32, error) {
	value, err := cursor.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}
func (cursor *checkpointCursor) u64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}
func (cursor *checkpointCursor) string(max int) (string, error) {
	length, err := cursor.u32()
	if err != nil || length > uint32(max) {
		return "", fmt.Errorf("%w: string length", ErrInvalidFileCheckpoint)
	}
	value, err := cursor.take(int(length))
	if err != nil || !utf8.Valid(value) {
		return "", fmt.Errorf("%w: invalid string", ErrInvalidFileCheckpoint)
	}
	return string(value), nil
}

type checkpointPayloadHeader struct {
	marker    string
	namespace string
}

type checkpointPayloadIdentity struct {
	recordID          FileCheckpointRecordID
	intentDigest      transfer.TransferIntentDigest
	fileID            catalog.FileID
	fileRevision      content.FileRevision
	canonicalPath     string
	exactSize         uint64
	backendID         transfer.OutputBackendID
	rootIdentity      FileCheckpointRootID
	ownedOutputObject FileCheckpointObjectID
}

type checkpointPayloadState struct {
	stateGeneration       uint64
	checkpointGeneration  uint64
	verifiedRanges        []FileCheckpointRange
	phase                 FileCheckpointPhase
	commitState           FileCheckpointCommitState
	quarantineReason      QuarantineReason
	phaseBeforeQuarantine FilePhase
	retirementReason      RetirementReason
}

func decodeCheckpointPayload(payload []byte) (FileCheckpointV1, error) {
	cursor := checkpointCursor{bytes: payload}
	header, err := decodeCheckpointPayloadHeader(&cursor)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	identity, err := decodeCheckpointPayloadIdentity(&cursor)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	state, err := decodeCheckpointPayloadState(&cursor)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	if cursor.off != len(payload) {
		return FileCheckpointV1{}, ErrFileCheckpointNonCanonical
	}
	return FileCheckpointV1{
		ownershipMarker:       header.marker,
		namespace:             header.namespace,
		recordID:              identity.recordID,
		intentDigest:          identity.intentDigest,
		fileID:                identity.fileID,
		fileRevision:          identity.fileRevision,
		canonicalPath:         identity.canonicalPath,
		exactSize:             identity.exactSize,
		backendID:             identity.backendID,
		rootIdentity:          identity.rootIdentity,
		ownedOutputObject:     identity.ownedOutputObject,
		stateGeneration:       state.stateGeneration,
		checkpointGeneration:  state.checkpointGeneration,
		verifiedRanges:        state.verifiedRanges,
		phase:                 state.phase,
		commitState:           state.commitState,
		quarantineReason:      state.quarantineReason,
		phaseBeforeQuarantine: state.phaseBeforeQuarantine,
		retirementReason:      state.retirementReason,
	}, nil
}

func decodeCheckpointPayloadHeader(cursor *checkpointCursor) (checkpointPayloadHeader, error) {
	domain, err := cursor.string(len(fileCheckpointDomain))
	if err != nil || domain != fileCheckpointDomain {
		return checkpointPayloadHeader{}, fmt.Errorf("%w: domain", ErrFileCheckpointNonCanonical)
	}
	version, err := cursor.byte()
	if err != nil || version != FileCheckpointV1SchemaVersion {
		return checkpointPayloadHeader{}, fmt.Errorf("%w: schema version", ErrInvalidFileCheckpoint)
	}
	marker, err := cursor.string(maxCheckpointMarkerBytes)
	if err != nil {
		return checkpointPayloadHeader{}, err
	}
	namespace, err := cursor.string(maxCheckpointNamespace)
	if err != nil {
		return checkpointPayloadHeader{}, err
	}
	return checkpointPayloadHeader{marker: marker, namespace: namespace}, nil
}

func decodeCheckpointPayloadIdentity(cursor *checkpointCursor) (checkpointPayloadIdentity, error) {
	recordID, err := cursor.fixedID("record ID")
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	intentRaw, err := cursor.take(transfer.TransferIntentDigestBytes)
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	intent, err := transfer.TransferIntentDigestFromBytes(intentRaw)
	if err != nil {
		return checkpointPayloadIdentity{}, fmt.Errorf("%w: intent digest", ErrFileCheckpointBinding)
	}
	fileID, err := decodeCheckpointFileID(cursor)
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	revision, err := decodeCheckpointRevision(cursor)
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	path, err := cursor.string(maxCheckpointPathBytes)
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	exactSize, err := cursor.u64()
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	backend, err := cursor.string(maxCheckpointBackendBytes)
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	root, err := cursor.fixedID("root identity")
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	object, err := cursor.fixedID("owned output object")
	if err != nil {
		return checkpointPayloadIdentity{}, err
	}
	return checkpointPayloadIdentity{
		recordID:          FileCheckpointRecordID(recordID),
		intentDigest:      intent,
		fileID:            fileID,
		fileRevision:      revision,
		canonicalPath:     path,
		exactSize:         exactSize,
		backendID:         transfer.OutputBackendID(backend),
		rootIdentity:      FileCheckpointRootID(root),
		ownedOutputObject: FileCheckpointObjectID(object),
	}, nil
}

func decodeCheckpointFileID(cursor *checkpointCursor) (catalog.FileID, error) {
	raw, err := cursor.take(catalog.IdentityBytes)
	if err != nil {
		return catalog.FileID{}, err
	}
	fileID, err := catalog.FileIDFromBytes(raw)
	if err != nil || fileID.IsZero() {
		return catalog.FileID{}, fmt.Errorf("%w: file ID", ErrFileCheckpointBinding)
	}
	return fileID, nil
}

func decodeCheckpointRevision(cursor *checkpointCursor) (content.FileRevision, error) {
	raw, err := cursor.take(content.IdentityBytes)
	if err != nil {
		return content.FileRevision{}, err
	}
	revision, err := content.FileRevisionFromBytes(raw)
	if err != nil || revision.IsZero() {
		return content.FileRevision{}, fmt.Errorf("%w: file revision", ErrFileCheckpointBinding)
	}
	return revision, nil
}

func (cursor *checkpointCursor) fixedID(name string) ([sha256.Size]byte, error) {
	raw, err := cursor.take(sha256.Size)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return fixedCheckpointID(raw, name)
}

func decodeCheckpointPayloadState(cursor *checkpointCursor) (checkpointPayloadState, error) {
	stateGeneration, err := cursor.u64()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	checkpointGeneration, err := cursor.u64()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	ranges, err := decodeCheckpointRanges(cursor)
	if err != nil {
		return checkpointPayloadState{}, err
	}
	phase, err := cursor.byte()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	commitState, err := cursor.byte()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	quarantineReason, err := cursor.byte()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	phaseBeforeQuarantine, err := cursor.byte()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	retirementReason, err := cursor.byte()
	if err != nil {
		return checkpointPayloadState{}, err
	}
	return checkpointPayloadState{
		stateGeneration:       stateGeneration,
		checkpointGeneration:  checkpointGeneration,
		verifiedRanges:        ranges,
		phase:                 FileCheckpointPhase(phase),
		commitState:           FileCheckpointCommitState(commitState),
		quarantineReason:      QuarantineReason(quarantineReason),
		phaseBeforeQuarantine: FilePhase(phaseBeforeQuarantine),
		retirementReason:      RetirementReason(retirementReason),
	}, nil
}

func decodeCheckpointRanges(cursor *checkpointCursor) ([]FileCheckpointRange, error) {
	count, err := cursor.u32()
	if err != nil || count > maxCheckpointRanges {
		return nil, fmt.Errorf("%w: range count", ErrInvalidFileCheckpoint)
	}
	ranges := make([]FileCheckpointRange, count)
	for index := range ranges {
		ranges[index].Offset, err = cursor.u64()
		if err != nil {
			return nil, err
		}
		ranges[index].End, err = cursor.u64()
		if err != nil {
			return nil, err
		}
	}
	return ranges, nil
}
func writeCheckpointBytes(writer io.Writer, value []byte) {
	writeCheckpointU32(writer, uint32(len(value)))
	_, _ = writer.Write(value)
}
func writeCheckpointString(writer io.Writer, value string) {
	writeCheckpointBytes(writer, []byte(value))
}
func writeCheckpointU32(writer io.Writer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}
func writeCheckpointU64(writer io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

// CheckpointIdentityEqual compares only the immutable binding.  It is used by
// recovery and prevents a record for one output object from being replayed into
// another merely because the file path happened to match.
