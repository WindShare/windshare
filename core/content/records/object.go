package records

import (
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

// Encoder produces immutable payloads that a session can frame for any
// recipient. Live lease validation stays with the sender service.
type Encoder struct{}

func (Encoder) EncodeRevision(descriptor content.FileRevisionDescriptor) ([]byte, error) {
	if descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return nil, ErrInvalidRecord
	}
	return encodeRevision(descriptor)
}

func (Encoder) EncodeBlock(record BlockRecord) ([]byte, error) {
	if record.DataLength() == 0 {
		return nil, ErrInvalidRecord
	}
	return encodeBlock(record)
}

// Decoder interprets payloads delivered by the authenticated operation. The
// request and lease still determine which identities and sizes are admissible.
type Decoder struct {
	ShareInstance catalog.ShareInstance
}

func (d Decoder) DecodeRevision(file catalog.FileID, chunkSize uint32, payload []byte) (content.FileRevisionDescriptor, error) {
	if len(payload) > MaxRevisionPayloadBytes {
		return content.FileRevisionDescriptor{}, ErrObjectTooLarge
	}
	return decodeRevision(payload, d.ShareInstance, file, chunkSize)
}

func (d Decoder) DecodeBlock(descriptor content.FileRevisionDescriptor, index uint64, payload []byte) (BlockRecord, error) {
	if descriptor.ShareInstance() != d.ShareInstance {
		return BlockRecord{}, ErrObjectIdentity
	}
	if len(payload) > MaxBlockRecordObjectBytes {
		return BlockRecord{}, ErrObjectTooLarge
	}
	return decodeBlock(payload, descriptor, index)
}
