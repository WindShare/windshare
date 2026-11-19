package catalog

import (
	"bytes"
	"testing"
)

func TestReceivedDescriptorConstruction(t *testing.T) {
	share, _ := ShareInstanceFromBytes(bytes.Repeat([]byte{1}, IdentityBytes))
	root, _ := DirectoryIDFromBytes(bytes.Repeat([]byte{2}, IdentityBytes))
	descriptor, err := NewReceivedShareDescriptor(ReceivedDescriptorSpec{
		WireVersion: WireVersionV2, Suite: SuiteV2, ShareInstance: share, SyntheticRoot: root,
		ChunkSize: MinChunkSize, Capabilities: CapabilityCatalog | CapabilityRanges,
		SenderPublicKey: bytes.Repeat([]byte{3}, SenderPublicKeySize), PathPolicy: PathPolicy,
	})
	if err != nil || descriptor.ShareInstance() != share {
		t.Fatalf("received descriptor = %+v, %v", descriptor, err)
	}
	if _, err := NewReceivedShareDescriptor(ReceivedDescriptorSpec{}); err == nil {
		t.Fatal("zero received descriptor was accepted")
	}
}
