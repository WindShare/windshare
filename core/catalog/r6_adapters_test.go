package catalog

import (
	"bytes"
	"errors"
	"testing"
)

func TestReceivedDescriptorAndPageFunctionAdapters(t *testing.T) {
	share, _ := ShareInstanceFromBytes(bytes.Repeat([]byte{1}, IdentityBytes))
	root, _ := DirectoryIDFromBytes(bytes.Repeat([]byte{2}, IdentityBytes))
	descriptor, err := NewReceivedShareDescriptor(ReceivedDescriptorSpec{
		WireVersion: WireVersionV2, Suite: SuiteV2, ShareInstance: share, SyntheticRoot: root,
		ChunkSize: MinChunkSize, Capabilities: CapabilityCatalog | CapabilityRanges,
		SenderPublicKey: bytes.Repeat([]byte{3}, SenderPublicKeySize), PathPolicy: PathPolicyV1,
	})
	if err != nil || descriptor.ShareInstance() != share {
		t.Fatalf("received descriptor = %+v, %v", descriptor, err)
	}
	if _, err := NewReceivedShareDescriptor(ReceivedDescriptorSpec{}); err == nil {
		t.Fatal("zero received descriptor was accepted")
	}
	sentinel := errors.New("adapter sentinel")
	if _, err := (PageCommitterFunc(nil)).Commit(PageCommitInput{}); err == nil {
		t.Fatal("nil page committer was accepted")
	}
	committer := PageCommitterFunc(func(PageCommitInput) (PageCommitment, error) {
		return PageCommitment{1}, sentinel
	})
	commitment, err := committer.Commit(PageCommitInput{})
	if commitment[0] != 1 || !errors.Is(err, sentinel) {
		t.Fatalf("commit adapter = %x, %v", commitment, err)
	}
	sealed, _ := NewSealedPageObject([]byte("sealed"))
	sealer := PageSealerFunc(func(PageCommitInput) (SealedPageObject, error) { return sealed, nil })
	result, err := sealer.Seal(PageCommitInput{})
	if err != nil || result.EstimatedMemoryBytes() != uint64(len("sealed")) {
		t.Fatalf("seal adapter = %+v, %v", result, err)
	}
}
