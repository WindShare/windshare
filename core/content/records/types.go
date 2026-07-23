package records

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/senderobject"
)

const (
	SchemaVersion             = 1
	ObjectWireVersion         = senderobject.WireVersion
	ObjectNonceBytes          = senderobject.NonceBytes
	ObjectHeaderBytes         = senderobject.HeaderBytes
	ObjectSignatureBytes      = senderobject.SignatureBytes
	ObjectFixedOverheadBytes  = senderobject.FixedBytes
	ObjectAuthenticationBytes = senderobject.TagBytes
	MaxBlockRecordObjectBytes = senderobject.MaxBlockRecordBytes
	MaxSealsPerAEADKey        = uint64(1) << 32
)

const (
	RevisionObjectDomain = string(senderobject.DomainRevision)
	BlockRecordDomain    = string(senderobject.DomainBlockRecord)
)

var (
	ErrInvalidRecord      = errors.New("content record is invalid")
	ErrObjectMalformed    = errors.New("sender object is malformed")
	ErrObjectTooLarge     = errors.New("sender object exceeds its frozen limit")
	ErrObjectSignature    = errors.New("sender object signature is invalid")
	ErrObjectAuth         = errors.New("sender object authentication failed")
	ErrObjectIdentity     = errors.New("sender object has the wrong semantic identity")
	ErrNonCanonicalObject = errors.New("sender object plaintext is not canonical CBOR")
	ErrNonceSource        = errors.New("sender object nonce source failed")
	ErrSealLimit          = errors.New("sender object AEAD key seal limit is exhausted")
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
// semantic; RecordID is deliberately derived later from one concrete seal.
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

type SealedBlock struct {
	ID     RecordID
	Object []byte
}
