package osfs

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func wave2CheckpointSpec(t *testing.T) FileCheckpointSpec {
	t.Helper()
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x21}, receivecontract.StableIdentityBytes))
	intent, _ := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0x22}, transfer.ReceiveIntentDigestBytes))
	binding, _ := receivecontract.BindingDigestFromBytes(bytes.Repeat([]byte{0x23}, 32))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x24}, receivecontract.AuthorityRefBytes))
	var fileID catalog.FileID
	var revision content.FileRevision
	fileID[0], revision[0] = 0x25, 0x26
	return FileCheckpointSpec{
		OperationID: operation, ReceiveIntentDigest: intent,
		MaterializationBindingDigest: binding, FileID: fileID, FileRevision: revision,
		CanonicalPath: "folder/file.bin", ExactSize: 8,
		MaterializerKind: FileCheckpointMaterializerNativeTree,
		AuthorityRef:     authority.Bytes(), OwnedObjectID: bytes.Repeat([]byte{0x27}, 32),
		StateGeneration: 2, CheckpointGeneration: 1,
		VerifiedRanges: []FileCheckpointRange{{Offset: 0, End: 4}},
		Phase:          FileCheckpointActive, CommitState: FileCheckpointVerified,
	}
}

func TestFileCheckpointV2PublicBoundaryRoundTripsStableAuthority(t *testing.T) {
	spec := wave2CheckpointSpec(t)
	checkpoint, err := NewFileCheckpointV2(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Valid() || checkpoint.SchemaVersion() != FileCheckpointV2SchemaVersion ||
		checkpoint.OwnershipMarker() != FileCheckpointOwnershipMarker ||
		checkpoint.Namespace() != FileCheckpointNamespace ||
		checkpoint.OperationID() != spec.OperationID ||
		checkpoint.ReceiveIntentDigest() != spec.ReceiveIntentDigest ||
		checkpoint.MaterializationBindingDigest() != spec.MaterializationBindingDigest ||
		checkpoint.FileID() != spec.FileID || checkpoint.FileRevision() != spec.FileRevision ||
		checkpoint.CanonicalPath() != spec.CanonicalPath || checkpoint.ExactSize() != spec.ExactSize ||
		checkpoint.MaterializerKind() != spec.MaterializerKind ||
		checkpoint.OwnedObjectID().IsZero() || checkpoint.RecordID().IsZero() ||
		checkpoint.Checksum().IsZero() || checkpoint.StateGeneration() != 2 ||
		checkpoint.CheckpointGeneration() != 1 ||
		checkpoint.VerifiedRanges()[0] != spec.VerifiedRanges[0] ||
		checkpoint.Phase() != FileCheckpointActive ||
		checkpoint.CommitState() != FileCheckpointVerified {
		t.Fatalf("checkpoint accessors lost v2 authority: %+v", checkpoint)
	}
	encoded, err := EncodeFileCheckpointV2(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeFileCheckpointV2(encoded)
	restoredEncoded, encodeErr := EncodeFileCheckpointV2(restored)
	if err != nil || encodeErr != nil || !bytes.Equal(restoredEncoded, encoded) {
		t.Fatalf("checkpoint round trip = %v", err)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 1
	if _, err := DecodeFileCheckpointV2(tampered); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("tampered checkpoint error = %v", err)
	}
	if _, err := DecodeFileCheckpointV2(nil); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("empty checkpoint error = %v", err)
	}
	if _, err := EncodeFileCheckpointV2(FileCheckpointV2{}); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("zero checkpoint error = %v", err)
	}
}

func TestFileCheckpointOwnershipUsesMaterializerAndOpaqueAuthority(t *testing.T) {
	authority := bytes.Repeat([]byte{0x31}, receivecontract.AuthorityRefBytes)
	ownership, err := NewFileCheckpointOwnership(FileCheckpointOwnershipSpec{
		Materializer:        FileCheckpointMaterializerNativeTree,
		Certification:       FileCheckpointCertificationWindowsNTFSProcessRestart,
		AuthorityRef:        authority,
		RootOpenDisposition: FileCheckpointCallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeFileCheckpointOwnership(encoded)
	if err != nil || !restored.Valid() ||
		restored.MaterializerKind() != FileCheckpointMaterializerNativeTree ||
		restored.Certification() != FileCheckpointCertificationWindowsNTFSProcessRestart ||
		!bytes.Equal(restored.AuthorityRef().Bytes(), authority) ||
		restored.RootOpenDisposition() != FileCheckpointCallerProvidedContainer {
		t.Fatalf("ownership round trip = (%+v, %v)", restored, err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := DecodeFileCheckpointOwnership(encoded); !errors.Is(err, ErrFileCheckpointOwnershipChecksum) {
		t.Fatalf("tampered ownership error = %v", err)
	}
	if _, err := NewFileCheckpointOwnership(FileCheckpointOwnershipSpec{}); !errors.Is(err, ErrFileCheckpointOwnership) {
		t.Fatalf("zero ownership error = %v", err)
	}
}
