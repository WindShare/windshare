package records

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const (
	RevisionSchemaVersion     = 2
	SchemaVersion             = 1
	MaxRevisionPayloadBytes   = 16 << 10
	MaxBlockRecordObjectBytes = (4 << 20) + 512
)

var (
	ErrInvalidRecord      = errors.New("content record is invalid")
	ErrObjectTooLarge     = errors.New("content record exceeds its limit")
	ErrObjectIdentity     = errors.New("content record has the wrong identity")
	ErrNonCanonicalObject = errors.New("content record is not canonical CBOR")
)

type RecordID [catalog.IdentityBytes]byte

func RecordIDFromObject(object []byte) RecordID {
	digest := sha256.Sum256(object)
	var id RecordID
	copy(id[:], digest[:len(id)])
	return id
}

func RecordIDFromBytes(raw []byte) (RecordID, error) {
	var id RecordID
	if len(raw) != len(id) {
		return id, fmt.Errorf("%w: record ID has %d bytes", ErrInvalidRecord, len(raw))
	}
	copy(id[:], raw)
	return id, nil
}

func (id RecordID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id RecordID) IsZero() bool  { return id == RecordID{} }

// BlockRecord owns plaintext from exactly one file-local block. Its identity is
// semantic; RecordID commits to its canonical encoded payload.
type BlockRecord struct {
	descriptor content.FileRevisionDescriptor
	index      uint64
	data       []byte
}

func NewBlockRecord(descriptor content.FileRevisionDescriptor, index uint64, data []byte) (BlockRecord, error) {
	if descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return BlockRecord{}, ErrInvalidRecord
	}
	expected, err := descriptor.Geometry().BlockPlainLength(index)
	if err != nil {
		return BlockRecord{}, fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	if uint64(len(data)) != uint64(expected) {
		return BlockRecord{}, fmt.Errorf("%w: block %d has %d bytes, expected %d", ErrInvalidRecord, index, len(data), expected)
	}
	return BlockRecord{descriptor: descriptor, index: index, data: append([]byte(nil), data...)}, nil
}

func (r BlockRecord) Descriptor() content.FileRevisionDescriptor { return r.descriptor }
func (r BlockRecord) LocalBlockIndex() uint64                    { return r.index }
func (r BlockRecord) DataLength() int                            { return len(r.data) }
func (r BlockRecord) Data() []byte                               { return append([]byte(nil), r.data...) }
