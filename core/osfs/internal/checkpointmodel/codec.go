package checkpointmodel

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

func DecodeRecord(encoded []byte) (Record, error) {
	minimum := len(recordMagic) + 4 + sha256.Size + 1
	if len(encoded) < minimum || !bytes.Equal(encoded[:len(recordMagic)], []byte(recordMagic)) {
		return Record{}, fmt.Errorf("%w: envelope", ErrInvalidRecord)
	}
	payloadEnd := len(encoded) - sha256.Size
	declared := binary.BigEndian.Uint32(encoded[len(recordMagic) : len(recordMagic)+4])
	actual := payloadEnd - len(recordMagic) - 4
	if uint64(declared) != uint64(actual) {
		return Record{}, fmt.Errorf("%w: payload length", ErrInvalidRecord)
	}
	payload := encoded[len(recordMagic)+4 : payloadEnd]
	var supplied Checksum
	copy(supplied[:], encoded[payloadEnd:])
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
	var expected Checksum
	copy(expected[:], hash.Sum(nil))
	if supplied != expected {
		return Record{}, ErrRecordChecksum
	}
	record, err := decodeRecordPayload(payload)
	if err != nil {
		return Record{}, err
	}
	record.checksum = supplied
	if err := record.validate(); err != nil {
		return Record{}, err
	}
	if !bytes.Equal(record.canonicalPayload(), payload) {
		return Record{}, ErrRecordNonCanonical
	}
	return record, nil
}

type recordCursor struct {
	bytes []byte
	off   int
}

func (cursor *recordCursor) take(count int) ([]byte, error) {
	if count < 0 || cursor.off < 0 || count > len(cursor.bytes)-cursor.off {
		return nil, fmt.Errorf("%w: truncated payload", ErrInvalidRecord)
	}
	value := cursor.bytes[cursor.off : cursor.off+count]
	cursor.off += count
	return value, nil
}

func (cursor *recordCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *recordCursor) u32() (uint32, error) {
	value, err := cursor.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}

func (cursor *recordCursor) u64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (cursor *recordCursor) string(maximum int) (string, error) {
	length, err := cursor.u32()
	if err != nil || length > uint32(maximum) {
		return "", fmt.Errorf("%w: string length", ErrInvalidRecord)
	}
	value, err := cursor.take(int(length))
	if err != nil || !utf8.Valid(value) {
		return "", fmt.Errorf("%w: invalid string", ErrInvalidRecord)
	}
	return string(value), nil
}

type recordPayloadHeader struct {
	marker    string
	namespace string
}

type recordPayloadIdentity struct {
	recordID          RecordID
	intentDigest      transfer.TransferIntentDigest
	fileID            catalog.FileID
	fileRevision      content.FileRevision
	canonicalPath     string
	exactSize         uint64
	backendID         transfer.OutputBackendID
	rootIdentity      RootIdentity
	ownedOutputObject ObjectID
}

type recordPayloadState struct {
	stateGeneration      uint64
	checkpointGeneration uint64
	verifiedRanges       []Range
	phase                Phase
	commitState          CommitState
	quarantineReason     QuarantineReason
	quarantineOrigin     QuarantineOrigin
	retirementReason     RetirementReason
}

func decodeRecordPayload(payload []byte) (Record, error) {
	cursor := recordCursor{bytes: payload}
	header, err := decodeRecordPayloadHeader(&cursor)
	if err != nil {
		return Record{}, err
	}
	identity, err := decodeRecordPayloadIdentity(&cursor)
	if err != nil {
		return Record{}, err
	}
	state, err := decodeRecordPayloadState(&cursor)
	if err != nil {
		return Record{}, err
	}
	if cursor.off != len(payload) {
		return Record{}, ErrRecordNonCanonical
	}
	return Record{
		ownershipMarker:      header.marker,
		namespace:            header.namespace,
		recordID:             identity.recordID,
		intentDigest:         identity.intentDigest,
		fileID:               identity.fileID,
		fileRevision:         identity.fileRevision,
		canonicalPath:        identity.canonicalPath,
		exactSize:            identity.exactSize,
		backendID:            identity.backendID,
		rootIdentity:         identity.rootIdentity,
		ownedOutputObject:    identity.ownedOutputObject,
		stateGeneration:      state.stateGeneration,
		checkpointGeneration: state.checkpointGeneration,
		verifiedRanges:       state.verifiedRanges,
		phase:                state.phase,
		commitState:          state.commitState,
		quarantineReason:     state.quarantineReason,
		quarantineOrigin:     state.quarantineOrigin,
		retirementReason:     state.retirementReason,
	}, nil
}

func decodeRecordPayloadHeader(cursor *recordCursor) (recordPayloadHeader, error) {
	domain, err := cursor.string(len(recordDomain))
	if err != nil || domain != recordDomain {
		return recordPayloadHeader{}, fmt.Errorf("%w: domain", ErrRecordNonCanonical)
	}
	version, err := cursor.byte()
	if err != nil || version != SchemaVersion {
		return recordPayloadHeader{}, fmt.Errorf("%w: schema version", ErrInvalidRecord)
	}
	marker, err := cursor.string(maximumMarkerBytes)
	if err != nil {
		return recordPayloadHeader{}, err
	}
	namespace, err := cursor.string(maximumNamespaceBytes)
	if err != nil {
		return recordPayloadHeader{}, err
	}
	return recordPayloadHeader{marker: marker, namespace: namespace}, nil
}

func decodeRecordPayloadIdentity(cursor *recordCursor) (recordPayloadIdentity, error) {
	recordID, err := cursor.fixedID("record ID")
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	intentRaw, err := cursor.take(transfer.TransferIntentDigestBytes)
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	intent, err := transfer.TransferIntentDigestFromBytes(intentRaw)
	if err != nil {
		return recordPayloadIdentity{}, fmt.Errorf("%w: intent digest", ErrRecordBinding)
	}
	fileID, err := decodeRecordFileID(cursor)
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	revision, err := decodeRecordRevision(cursor)
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	path, err := cursor.string(maximumPathBytes)
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	exactSize, err := cursor.u64()
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	backend, err := cursor.string(maximumBackendBytes)
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	root, err := cursor.fixedID("root identity")
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	object, err := cursor.fixedID("owned output object")
	if err != nil {
		return recordPayloadIdentity{}, err
	}
	return recordPayloadIdentity{
		recordID:          RecordID(recordID),
		intentDigest:      intent,
		fileID:            fileID,
		fileRevision:      revision,
		canonicalPath:     path,
		exactSize:         exactSize,
		backendID:         transfer.OutputBackendID(backend),
		rootIdentity:      RootIdentity(root),
		ownedOutputObject: ObjectID(object),
	}, nil
}

func decodeRecordFileID(cursor *recordCursor) (catalog.FileID, error) {
	raw, err := cursor.take(catalog.IdentityBytes)
	if err != nil {
		return catalog.FileID{}, err
	}
	fileID, err := catalog.FileIDFromBytes(raw)
	if err != nil || fileID.IsZero() {
		return catalog.FileID{}, fmt.Errorf("%w: file ID", ErrRecordBinding)
	}
	return fileID, nil
}

func decodeRecordRevision(cursor *recordCursor) (content.FileRevision, error) {
	raw, err := cursor.take(content.IdentityBytes)
	if err != nil {
		return content.FileRevision{}, err
	}
	revision, err := content.FileRevisionFromBytes(raw)
	if err != nil || revision.IsZero() {
		return content.FileRevision{}, fmt.Errorf("%w: file revision", ErrRecordBinding)
	}
	return revision, nil
}

func (cursor *recordCursor) fixedID(name string) ([sha256.Size]byte, error) {
	raw, err := cursor.take(sha256.Size)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return fixedID(raw, name)
}

func decodeRecordPayloadState(cursor *recordCursor) (recordPayloadState, error) {
	stateGeneration, err := cursor.u64()
	if err != nil {
		return recordPayloadState{}, err
	}
	checkpointGeneration, err := cursor.u64()
	if err != nil {
		return recordPayloadState{}, err
	}
	ranges, err := decodeRecordRanges(cursor)
	if err != nil {
		return recordPayloadState{}, err
	}
	phase, err := cursor.byte()
	if err != nil {
		return recordPayloadState{}, err
	}
	commitState, err := cursor.byte()
	if err != nil {
		return recordPayloadState{}, err
	}
	quarantineReason, err := cursor.byte()
	if err != nil {
		return recordPayloadState{}, err
	}
	quarantineOrigin, err := cursor.byte()
	if err != nil {
		return recordPayloadState{}, err
	}
	retirementReason, err := cursor.byte()
	if err != nil {
		return recordPayloadState{}, err
	}
	return recordPayloadState{
		stateGeneration:      stateGeneration,
		checkpointGeneration: checkpointGeneration,
		verifiedRanges:       ranges,
		phase:                Phase(phase),
		commitState:          CommitState(commitState),
		quarantineReason:     QuarantineReason(quarantineReason),
		quarantineOrigin:     QuarantineOrigin(quarantineOrigin),
		retirementReason:     RetirementReason(retirementReason),
	}, nil
}

func decodeRecordRanges(cursor *recordCursor) ([]Range, error) {
	count, err := cursor.u32()
	if err != nil || count > maximumRanges {
		return nil, fmt.Errorf("%w: range count", ErrInvalidRecord)
	}
	ranges := make([]Range, count)
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

func writeRecordBytes(writer io.Writer, value []byte) {
	writeRecordU32(writer, uint32(len(value)))
	_, _ = writer.Write(value)
}

func writeRecordString(writer io.Writer, value string) {
	writeRecordBytes(writer, []byte(value))
}

func writeRecordU32(writer io.Writer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeRecordU64(writer io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}
