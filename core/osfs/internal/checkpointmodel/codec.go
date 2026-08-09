package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func DecodeRecord(encoded []byte) (Record, error) {
	minimum := len(recordMagic) + 4 + sha256.Size + len(recordDomain) + 2
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
	expected := checksumPayload(payload)
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

func checksumPayload(payload []byte) Checksum {
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0, SchemaVersion})
	writeRecordFrame(hash, payload)
	var checksum Checksum
	copy(checksum[:], hash.Sum(nil))
	return checksum
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

func (cursor *recordCursor) rawU64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (cursor *recordCursor) frame(maximum uint64) ([]byte, error) {
	length, err := cursor.rawU64()
	if err != nil || length > maximum || length > uint64(len(cursor.bytes)-cursor.off) {
		return nil, fmt.Errorf("%w: invalid frame", ErrInvalidRecord)
	}
	return cursor.take(int(length))
}

func (cursor *recordCursor) framedByte() (byte, error) {
	value, err := cursor.frame(1)
	if err != nil || len(value) != 1 {
		return 0, fmt.Errorf("%w: invalid byte frame", ErrInvalidRecord)
	}
	return value[0], nil
}

func (cursor *recordCursor) framedU64() (uint64, error) {
	value, err := cursor.frame(8)
	if err != nil || len(value) != 8 {
		return 0, fmt.Errorf("%w: invalid u64 frame", ErrInvalidRecord)
	}
	return binary.BigEndian.Uint64(value), nil
}

func (cursor *recordCursor) text(maximum uint64) (string, error) {
	value, err := cursor.frame(maximum)
	if err != nil || !utf8.Valid(value) {
		return "", fmt.Errorf("%w: invalid text frame", ErrInvalidRecord)
	}
	return string(value), nil
}

func decodeRecordPayload(payload []byte) (Record, error) {
	cursor := recordCursor{bytes: payload}
	prefix, err := cursor.take(len(recordDomain) + 2)
	if err != nil || !bytes.Equal(prefix, append(append([]byte(nil), []byte(recordDomain)...), 0, SchemaVersion)) {
		return Record{}, fmt.Errorf("%w: domain or schema version", ErrRecordNonCanonical)
	}
	marker, err := cursor.text(maximumMarkerBytes)
	if err != nil {
		return Record{}, err
	}
	namespace, err := cursor.text(maximumNamespaceBytes)
	if err != nil {
		return Record{}, err
	}
	recordID, err := decodeRecordID(&cursor)
	if err != nil {
		return Record{}, err
	}
	operation, err := decodeOperationID(&cursor)
	if err != nil {
		return Record{}, err
	}
	receiveIntent, err := decodeReceiveIntentDigest(&cursor)
	if err != nil {
		return Record{}, err
	}
	materialization, err := decodeBindingDigest(&cursor)
	if err != nil {
		return Record{}, err
	}
	fileID, err := decodeRecordFileID(&cursor)
	if err != nil {
		return Record{}, err
	}
	revision, err := decodeRecordRevision(&cursor)
	if err != nil {
		return Record{}, err
	}
	pathBytes, err := cursor.frame(maximumPathBytes + uint64(catalog.MaxPathDepth+1)*8)
	if err != nil {
		return Record{}, err
	}
	path, err := decodeCanonicalPath(pathBytes)
	if err != nil {
		return Record{}, err
	}
	exactSize, err := cursor.framedU64()
	if err != nil {
		return Record{}, err
	}
	materializer, err := cursor.framedByte()
	if err != nil {
		return Record{}, err
	}
	authority, err := decodeAuthorityRef(&cursor)
	if err != nil {
		return Record{}, err
	}
	object, err := decodeObjectID(&cursor)
	if err != nil {
		return Record{}, err
	}
	stateGeneration, err := cursor.framedU64()
	if err != nil {
		return Record{}, err
	}
	checkpointGeneration, err := cursor.framedU64()
	if err != nil {
		return Record{}, err
	}
	ranges, err := decodeRecordRanges(&cursor)
	if err != nil {
		return Record{}, err
	}
	phase, err := cursor.framedByte()
	if err != nil {
		return Record{}, err
	}
	commit, err := cursor.framedByte()
	if err != nil {
		return Record{}, err
	}
	quarantineReason, err := cursor.framedByte()
	if err != nil {
		return Record{}, err
	}
	quarantineOrigin, err := cursor.framedByte()
	if err != nil {
		return Record{}, err
	}
	retirementReason, err := cursor.framedByte()
	if err != nil || cursor.off != len(payload) {
		return Record{}, ErrRecordNonCanonical
	}
	return Record{
		ownershipMarker: marker, namespace: namespace, recordID: recordID,
		operationID: operation, receiveIntentDigest: receiveIntent,
		materializationBindingDigest: materialization, fileID: fileID,
		fileRevision: revision, canonicalPath: path, exactSize: exactSize,
		materializerKind: MaterializerKind(materializer), authorityRef: authority,
		ownedObjectID: object, stateGeneration: stateGeneration,
		checkpointGeneration: checkpointGeneration, verifiedRanges: ranges,
		phase: Phase(phase), commitState: CommitState(commit),
		quarantineReason: QuarantineReason(quarantineReason),
		quarantineOrigin: QuarantineOrigin(quarantineOrigin),
		retirementReason: RetirementReason(retirementReason),
	}, nil
}

func decodeRecordID(cursor *recordCursor) (RecordID, error) {
	raw, err := cursor.frame(sha256.Size)
	if err != nil || len(raw) != sha256.Size {
		return RecordID{}, fmt.Errorf("%w: record ID", ErrRecordBinding)
	}
	return RecordIDFromBytes(raw)
}

func decodeOperationID(cursor *recordCursor) (receivecontract.OperationID, error) {
	raw, err := cursor.frame(receivecontract.StableIdentityBytes)
	if err != nil || len(raw) != receivecontract.StableIdentityBytes {
		return receivecontract.OperationID{}, fmt.Errorf("%w: operation ID", ErrRecordBinding)
	}
	value, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		return receivecontract.OperationID{}, fmt.Errorf("%w: operation ID", ErrRecordBinding)
	}
	return value, nil
}

func decodeReceiveIntentDigest(cursor *recordCursor) (transfer.ReceiveIntentDigest, error) {
	raw, err := cursor.frame(transfer.ReceiveIntentDigestBytes)
	if err != nil || len(raw) != transfer.ReceiveIntentDigestBytes {
		return transfer.ReceiveIntentDigest{}, fmt.Errorf("%w: receive intent digest", ErrRecordBinding)
	}
	value, err := transfer.ReceiveIntentDigestFromBytes(raw)
	if err != nil {
		return transfer.ReceiveIntentDigest{}, fmt.Errorf("%w: receive intent digest", ErrRecordBinding)
	}
	return value, nil
}

func decodeBindingDigest(cursor *recordCursor) (receivecontract.BindingDigest, error) {
	raw, err := cursor.frame(sha256.Size)
	if err != nil || len(raw) != sha256.Size {
		return receivecontract.BindingDigest{}, fmt.Errorf("%w: materialization binding", ErrRecordBinding)
	}
	value, err := receivecontract.BindingDigestFromBytes(raw)
	if err != nil {
		return receivecontract.BindingDigest{}, fmt.Errorf("%w: materialization binding", ErrRecordBinding)
	}
	return value, nil
}

func decodeRecordFileID(cursor *recordCursor) (catalog.FileID, error) {
	raw, err := cursor.frame(catalog.IdentityBytes)
	if err != nil || len(raw) != catalog.IdentityBytes {
		return catalog.FileID{}, fmt.Errorf("%w: file ID", ErrRecordBinding)
	}
	fileID, err := catalog.FileIDFromBytes(raw)
	if err != nil || fileID.IsZero() {
		return catalog.FileID{}, fmt.Errorf("%w: file ID", ErrRecordBinding)
	}
	return fileID, nil
}

func decodeRecordRevision(cursor *recordCursor) (content.FileRevision, error) {
	raw, err := cursor.frame(content.IdentityBytes)
	if err != nil || len(raw) != content.IdentityBytes {
		return content.FileRevision{}, fmt.Errorf("%w: file revision", ErrRecordBinding)
	}
	revision, err := content.FileRevisionFromBytes(raw)
	if err != nil || revision.IsZero() {
		return content.FileRevision{}, fmt.Errorf("%w: file revision", ErrRecordBinding)
	}
	return revision, nil
}

func decodeAuthorityRef(cursor *recordCursor) (receivecontract.AuthorityRef, error) {
	raw, err := cursor.frame(receivecontract.AuthorityRefBytes)
	if err != nil || len(raw) != receivecontract.AuthorityRefBytes {
		return receivecontract.AuthorityRef{}, fmt.Errorf("%w: authority reference", ErrRecordBinding)
	}
	value, err := receivecontract.AuthorityRefFromBytes(raw)
	if err != nil {
		return receivecontract.AuthorityRef{}, fmt.Errorf("%w: authority reference", ErrRecordBinding)
	}
	return value, nil
}

func decodeObjectID(cursor *recordCursor) (ObjectID, error) {
	raw, err := cursor.frame(sha256.Size)
	if err != nil || len(raw) != sha256.Size {
		return ObjectID{}, fmt.Errorf("%w: owned object ID", ErrRecordBinding)
	}
	return ObjectIDFromBytes(raw)
}

func decodeRecordRanges(cursor *recordCursor) ([]Range, error) {
	count, err := cursor.rawU64()
	if err != nil || count > maximumRanges {
		return nil, fmt.Errorf("%w: range count", ErrInvalidRecord)
	}
	ranges := make([]Range, int(count))
	for index := range ranges {
		ranges[index].Offset, err = cursor.framedU64()
		if err != nil {
			return nil, err
		}
		ranges[index].End, err = cursor.framedU64()
		if err != nil {
			return nil, err
		}
	}
	return ranges, nil
}

func canonicalPathBytes(path string) []byte {
	components := strings.Split(path, "/")
	var encoded bytes.Buffer
	writeRecordU64(&encoded, uint64(len(components)))
	for _, component := range components {
		writeRecordFrame(&encoded, []byte(component))
	}
	return encoded.Bytes()
}

func decodeCanonicalPath(encoded []byte) (string, error) {
	cursor := recordCursor{bytes: encoded}
	count, err := cursor.rawU64()
	if err != nil || count == 0 || count > catalog.MaxPathDepth {
		return "", fmt.Errorf("%w: canonical path count", ErrRecordBinding)
	}
	components := make([]string, int(count))
	for index := range components {
		component, err := cursor.text(catalog.MaxPathBytes)
		if err != nil || component == "" || strings.ContainsRune(component, '/') {
			return "", fmt.Errorf("%w: canonical path component", ErrRecordBinding)
		}
		components[index] = component
	}
	if cursor.off != len(encoded) {
		return "", ErrRecordNonCanonical
	}
	path := strings.Join(components, "/")
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path || !bytes.Equal(canonicalPathBytes(path), encoded) {
		return "", fmt.Errorf("%w: canonical path", ErrRecordBinding)
	}
	return path, nil
}

func writeRecordPrefix(writer io.Writer) {
	_, _ = writer.Write([]byte(recordDomain))
	_, _ = writer.Write([]byte{0, SchemaVersion})
}

func writeRecordFrame(writer io.Writer, value []byte) {
	writeRecordU64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeRecordFramedU64(writer io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeRecordFrame(writer, encoded[:])
}

func writeRecordU64(writer io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}
