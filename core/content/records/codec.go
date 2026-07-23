package records

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

var recordEncMode = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var recordDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  4,
		MaxArrayElements: 16,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func encodeRevision(descriptor content.FileRevisionDescriptor) ([]byte, error) {
	modified := descriptor.ModifiedTime()
	var seconds any
	var nanos uint64
	var precision uint64
	if modified.Present() {
		seconds = modified.Seconds()
		nanos = uint64(modified.Nanoseconds())
		precision = uint64(modified.Precision())
	}
	return recordEncMode.Marshal(map[uint64]any{
		0: uint64(SchemaVersion),
		1: descriptor.ShareInstance().Bytes(),
		2: descriptor.FileID().Bytes(),
		3: descriptor.FileRevision().Bytes(),
		4: descriptor.ExactSize(),
		5: seconds,
		6: nanos,
		7: precision,
	})
}

func decodeRevision(plaintext []byte, expectedShare catalog.ShareInstance, expectedFile catalog.FileID, chunkSize uint32) (content.FileRevisionDescriptor, error) {
	fields, err := decodeMap(plaintext, 8)
	if err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	if err := requireSchema(fields[0]); err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	share, err := decodeShare(fields[1])
	if err != nil || share != expectedShare {
		return content.FileRevisionDescriptor{}, ErrObjectIdentity
	}
	file, err := decodeFile(fields[2])
	if err != nil || file != expectedFile {
		return content.FileRevisionDescriptor{}, ErrObjectIdentity
	}
	revisionBytes, err := decodeBytes(fields[3])
	if err != nil {
		return content.FileRevisionDescriptor{}, ErrInvalidRecord
	}
	revision, err := content.FileRevisionFromBytes(revisionBytes)
	if err != nil || revision.IsZero() {
		return content.FileRevisionDescriptor{}, ErrInvalidRecord
	}
	exactSize, err := decodeUint(fields[4])
	if err != nil || exactSize > catalog.MaxFileSize {
		return content.FileRevisionDescriptor{}, ErrInvalidRecord
	}
	modified, err := decodeModified(fields[5], fields[6], fields[7])
	if err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	geometry, err := content.NewFileGeometry(exactSize, chunkSize)
	if err != nil {
		return content.FileRevisionDescriptor{}, err
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, modified)
	if err != nil {
		return content.FileRevisionDescriptor{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	return descriptor, nil
}

func encodeBlock(record BlockRecord) ([]byte, error) {
	descriptor := record.descriptor
	return recordEncMode.Marshal(map[uint64]any{
		0: uint64(SchemaVersion),
		1: descriptor.ShareInstance().Bytes(),
		2: descriptor.FileID().Bytes(),
		3: descriptor.FileRevision().Bytes(),
		4: record.index,
		5: record.data,
	})
}

func decodeBlock(plaintext []byte, expected content.FileRevisionDescriptor, index uint64) (BlockRecord, error) {
	fields, err := decodeMap(plaintext, 6)
	if err != nil {
		return BlockRecord{}, err
	}
	if err := requireSchema(fields[0]); err != nil {
		return BlockRecord{}, err
	}
	share, err := decodeShare(fields[1])
	if err != nil || share != expected.ShareInstance() {
		return BlockRecord{}, ErrObjectIdentity
	}
	file, err := decodeFile(fields[2])
	if err != nil || file != expected.FileID() {
		return BlockRecord{}, ErrObjectIdentity
	}
	revisionBytes, err := decodeBytes(fields[3])
	if err != nil {
		return BlockRecord{}, ErrInvalidRecord
	}
	revision, err := content.FileRevisionFromBytes(revisionBytes)
	if err != nil || revision != expected.FileRevision() {
		return BlockRecord{}, ErrObjectIdentity
	}
	decodedIndex, err := decodeUint(fields[4])
	if err != nil || decodedIndex != index {
		return BlockRecord{}, ErrObjectIdentity
	}
	data, err := decodeBytes(fields[5])
	if err != nil {
		return BlockRecord{}, ErrInvalidRecord
	}
	return NewBlockRecord(expected, index, data)
}

func decodeMap(plaintext []byte, expectedFields int) (map[uint64]cbor.RawMessage, error) {
	if len(plaintext) == 0 {
		return nil, ErrInvalidRecord
	}
	var fields map[uint64]cbor.RawMessage
	if err := recordDecMode.Unmarshal(plaintext, &fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if len(fields) != expectedFields {
		return nil, ErrInvalidRecord
	}
	for key := range expectedFields {
		if fields[uint64(key)] == nil {
			return nil, ErrInvalidRecord
		}
	}
	var decoded any
	if err := recordDecMode.Unmarshal(plaintext, &decoded); err != nil {
		return nil, ErrInvalidRecord
	}
	canonical, err := recordEncMode.Marshal(decoded)
	if err != nil || !bytes.Equal(canonical, plaintext) {
		return nil, ErrNonCanonicalObject
	}
	return fields, nil
}

func requireSchema(encoded cbor.RawMessage) error {
	value, err := decodeUint(encoded)
	if err != nil || value != SchemaVersion {
		return ErrInvalidRecord
	}
	return nil
}

func decodeShare(encoded cbor.RawMessage) (catalog.ShareInstance, error) {
	raw, err := decodeBytes(encoded)
	if err != nil {
		return catalog.ShareInstance{}, err
	}
	return catalog.ShareInstanceFromBytes(raw)
}

func decodeFile(encoded cbor.RawMessage) (catalog.FileID, error) {
	raw, err := decodeBytes(encoded)
	if err != nil {
		return catalog.FileID{}, err
	}
	return catalog.FileIDFromBytes(raw)
}

func decodeBytes(encoded cbor.RawMessage) ([]byte, error) {
	var value []byte
	if err := recordDecMode.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeUint(encoded cbor.RawMessage) (uint64, error) {
	var value uint64
	if err := recordDecMode.Unmarshal(encoded, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func decodeModified(secondsCBOR, nanosCBOR, precisionCBOR cbor.RawMessage) (catalog.ModifiedTime, error) {
	nanos, err := decodeUint(nanosCBOR)
	if err != nil || nanos > math.MaxUint32 {
		return catalog.ModifiedTime{}, ErrInvalidRecord
	}
	precision, err := decodeUint(precisionCBOR)
	if err != nil || precision > math.MaxUint8 {
		return catalog.ModifiedTime{}, ErrInvalidRecord
	}
	if bytes.Equal(secondsCBOR, []byte{0xf6}) {
		if nanos != 0 || precision != 0 {
			return catalog.ModifiedTime{}, ErrInvalidRecord
		}
		return catalog.ModifiedTime{}, nil
	}
	var seconds int64
	if err := recordDecMode.Unmarshal(secondsCBOR, &seconds); err != nil {
		return catalog.ModifiedTime{}, ErrInvalidRecord
	}
	modified, err := catalog.NewModifiedTime(seconds, uint32(nanos), catalog.TimePrecision(precision))
	if err != nil {
		return catalog.ModifiedTime{}, errors.Join(ErrInvalidRecord, err)
	}
	return modified, nil
}
