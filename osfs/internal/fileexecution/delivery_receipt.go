package fileexecution

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

// DeliveryReceipt records historical delivery only. It cannot authorize writes,
// deletion, or reuse of bytes in the current public namespace.
type DeliveryReceipt struct {
	DescriptorDigest [sha256.Size]byte
	Object           transfer.OwnedObjectID
}

// DeliveryReceipts belongs to one live operation, never to restart recovery.
// Implementations must preserve earlier receipts when a write fails.
type DeliveryReceipts interface {
	Lookup(transfer.MaterializationLocatorDigest) (DeliveryReceipt, bool, error)
	Record(transfer.MaterializationLocatorDigest, DeliveryReceipt) error
	Close() error
}

func deliveredFile(binding transfer.MaterializedFileBinding) DeliveryReceipt {
	return DeliveryReceipt{DescriptorDigest: deliveryDescriptorDigest(binding.Descriptor()), Object: binding.ObjectIdentity()}
}

func deliveryDescriptorDigest(descriptor content.FileRevisionDescriptor) [sha256.Size]byte {
	encoded := []byte("windshare/live-delivery-descriptor/v1\x00")
	share, file, revision := descriptor.ShareInstance(), descriptor.FileID(), descriptor.FileRevision()
	encoded = append(encoded, share[:]...)
	encoded = append(encoded, file[:]...)
	encoded = append(encoded, revision[:]...)
	encoded = binary.BigEndian.AppendUint64(encoded, descriptor.ExactSize())
	encoded = binary.BigEndian.AppendUint32(encoded, descriptor.Geometry().ChunkSize())
	modified := descriptor.ModifiedTime()
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(modified.Seconds()))
	encoded = binary.BigEndian.AppendUint32(encoded, modified.Nanoseconds())
	encoded = append(encoded, byte(modified.Precision()))
	if modified.Present() {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	return sha256.Sum256(encoded)
}
