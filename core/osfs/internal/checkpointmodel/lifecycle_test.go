package checkpointmodel

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestLifecycleRoundTripReferencesCheckpointCutsWithoutRanges(t *testing.T) {
	intent, authority := receiveOperationIntentFixture(t, 0x61)
	reference := lifecycleCheckpointReference(t, intent.OperationID(), intent.Digest(), intent.BindingDigest(), authority, 0x71)
	evidence, err := AggregateDigestFromBytes(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptPartialDirectory, OperationID: intent.OperationID(),
		ReceiveIntent: intent.Digest(), ReservationDigest: intent.BindingDigest(),
		CheckpointRefs: []FileCheckpointReference{reference}, EvidenceDigest: evidence,
		SuccessCount: 1, FailureCount: 1, PartialReason: PartialDirectoryFailures,
	})
	if err != nil {
		t.Fatal(err)
	}
	restoredReceipt, err := DecodeDirectTreeReceipt(receipt.CanonicalBytes())
	if err != nil || restoredReceipt.Digest() != receipt.Digest() ||
		restoredReceipt.CheckpointReferences()[0] != reference {
		t.Fatalf("receipt round trip = (%+v, %v)", restoredReceipt, err)
	}
	expires, err := NextStableExpiry(1_000)
	if err != nil || expires != 1_000+StableRetentionMilliseconds {
		t.Fatalf("stable expiry = %d, %v", expires, err)
	}
	lifecycle, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(),
		StateGeneration: 3, Phase: LifecycleResumableReceive,
		CheckpointRefs:  []FileCheckpointReference{reference},
		ExpiresAtMillis: expires, SuccessCount: 1, FailureCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReceiveLifecycleState(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeReceiveLifecycleState(encoded)
	if err != nil || restored.OperationID() != intent.OperationID() ||
		restored.ReceiveIntentDigest() != intent.Digest() ||
		restored.StateGeneration() != 3 || restored.Phase() != LifecycleResumableReceive ||
		restored.ExpiresAtMillis() != expires || restored.SuccessCount() != 1 ||
		restored.FailureCount() != 1 || restored.CheckpointReferences()[0] != reference {
		t.Fatalf("lifecycle round trip = (%+v, %v)", restored, err)
	}
	if bytes.Contains(encoded, []byte("folder/file.bin")) {
		t.Fatal("aggregate lifecycle duplicated file checkpoint path/range authority")
	}
}

func TestLifecycleTerminalStatesCarryClosedReasons(t *testing.T) {
	intent, _ := receiveOperationIntentFixture(t, 0x41)
	receipt, _ := AggregateDigestFromBytes(bytes.Repeat([]byte{0x51}, 32))
	for name, spec := range map[string]LifecycleStateSpec{
		"published": {
			OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 4,
			Phase: LifecyclePublished, ReceiptDigest: receipt, CleanupState: OwnedCleanupPending,
		},
		"partial": {
			OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 4,
			Phase: LifecyclePartialDirectory, ReceiptDigest: receipt, SuccessCount: 2, FailureCount: 1,
			PartialReason: PartialDirectoryStopped,
		},
		"discarded": {
			OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 4,
			Phase: LifecycleDiscarded, ReceiptDigest: receipt, CleanupState: OwnedCleanupClean,
		},
		"expired": {
			OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 4,
			Phase: LifecycleExpired, ReceiptDigest: receipt, ExpiresAtMillis: 50,
			CleanupState: OwnedCleanupPending, PriorStableState: LifecycleResumableReceive,
		},
	} {
		t.Run(name, func(t *testing.T) {
			record, err := NewReceiveLifecycleState(spec)
			if err != nil || !record.Valid() {
				t.Fatalf("terminal lifecycle = %+v, %v", record, err)
			}
		})
	}
	for reason, want := range map[NeedsAttentionReason]string{
		AttentionTargetOwnershipUnknown: "target-ownership-unknown",
		AttentionPublicationUnknown:     "publication-unknown",
		AttentionCleanupUnknown:         "cleanup-unknown",
	} {
		state, err := NewReceiveLifecycleState(LifecycleStateSpec{
			OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 5,
			Phase: LifecycleNeedsAttention, ReceiptDigest: receipt, AttentionReason: reason,
		})
		if err != nil || state.AttentionReason().String() != want {
			t.Fatalf("attention %d = %q, %v", reason, state.AttentionReason().String(), err)
		}
	}
}

func TestCheckpointReferencePreservesCommittedZeroByteGeneration(t *testing.T) {
	intent, authority := receiveOperationIntentFixture(t, 0x42)
	var fileID catalog.FileID
	var revision content.FileRevision
	fileID[0], revision[0] = 0x43, 0x44
	candidate, err := NewRecord(RecordSpec{
		OperationID: intent.OperationID(), ReceiveIntentDigest: intent.Digest(),
		MaterializationBindingDigest: intent.BindingDigest(), FileID: fileID,
		FileRevision: revision, CanonicalPath: "empty.bin", ExactSize: 0,
		MaterializerKind: MaterializerNativeTree, AuthorityRef: authority.Bytes(),
		OwnedObjectID: bytes.Repeat([]byte{0x45}, 32), StateGeneration: 1,
		CheckpointGeneration: 0, Phase: PhaseActive, CommitState: CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewFileCheckpointReference(verified)
	if err != nil || reference.CheckpointGeneration() != 0 {
		t.Fatalf("zero-byte reference = (%+v, %v)", reference, err)
	}
	quarantined, err := AdvanceState(
		verified, 2, PhaseQuarantined, CommitQuarantined,
		QuarantineAnchorUnsafe, QuarantineOriginWitnessed, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileCheckpointReference(quarantined); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("quarantined checkpoint reference error = %v", err)
	}
	evidence, _ := AggregateDigestFromBytes(bytes.Repeat([]byte{0x46}, 32))
	receipt, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptTreeCompletion, OperationID: intent.OperationID(),
		ReceiveIntent: intent.Digest(), ReservationDigest: intent.BindingDigest(),
		CheckpointRefs: []FileCheckpointReference{reference}, EvidenceDigest: evidence,
		SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeDirectTreeReceipt(receipt.CanonicalBytes())
	if err != nil || len(restored.CheckpointReferences()) != 1 ||
		restored.CheckpointReferences()[0] != reference {
		t.Fatalf("zero-byte receipt round trip = (%+v, %v)", restored, err)
	}
}

func TestLifecycleRejectsDuplicateAuthorityAndNonCanonicalImages(t *testing.T) {
	intent, authority := receiveOperationIntentFixture(t, 0x31)
	reference := lifecycleCheckpointReference(t, intent.OperationID(), intent.Digest(), intent.BindingDigest(), authority, 0x32)
	if _, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 1,
		Phase: LifecycleReceiving, CheckpointRefs: []FileCheckpointReference{reference, reference},
	}); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("duplicate reference error = %v", err)
	}
	if _, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 1,
		Phase: LifecycleResumableReceive,
	}); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("deadline-free resumable state error = %v", err)
	}
	receiptDigest, _ := AggregateDigestFromBytes(bytes.Repeat([]byte{0x33}, 32))
	if _, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 1,
		Phase: LifecyclePartialDirectory, ReceiptDigest: receiptDigest, SuccessCount: 1,
		PartialReason: PartialDirectoryFailures,
	}); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("failure-free partial lifecycle error = %v", err)
	}
	if _, err := NewDirectTreeReceipt(DirectTreeReceiptSpec{
		Kind: ReceiptPartialDirectory, OperationID: intent.OperationID(),
		ReceiveIntent: intent.Digest(), ReservationDigest: intent.BindingDigest(),
		EvidenceDigest: receiptDigest, SuccessCount: 1, PartialReason: PartialDirectoryFailures,
	}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("failure-free partial receipt error = %v", err)
	}
	valid, err := NewReceiveLifecycleState(LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(), StateGeneration: 1,
		Phase: LifecycleIntentFrozen,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := EncodeReceiveLifecycleState(valid)
	for name, image := range map[string][]byte{
		"truncated": encoded[:len(encoded)-1],
		"trailing":  append(append([]byte(nil), encoded...), 0),
		"foreign":   append([]byte("foreign"), encoded...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeReceiveLifecycleState(image); !errors.Is(err, ErrInvalidLifecycleState) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
	if _, err := NextStableExpiry(math.MaxUint64); !errors.Is(err, ErrInvalidLifecycleState) {
		t.Fatalf("expiry overflow error = %v", err)
	}
}

func lifecycleCheckpointReference(
	t *testing.T,
	operation receivecontract.OperationID,
	intent [32]byte,
	binding receivecontract.BindingDigest,
	authority receivecontract.AuthorityRef,
	seed byte,
) FileCheckpointReference {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	fileID[0], revision[0] = seed, seed+1
	candidate, err := NewRecord(RecordSpec{
		OperationID: operation, ReceiveIntentDigest: intent,
		MaterializationBindingDigest: binding, FileID: fileID, FileRevision: revision,
		CanonicalPath: "folder/file.bin", ExactSize: 4,
		MaterializerKind: MaterializerNativeTree, AuthorityRef: authority.Bytes(),
		OwnedObjectID: bytes.Repeat([]byte{seed + 2}, 32), StateGeneration: 1,
		CheckpointGeneration: 1, VerifiedRanges: []Range{{Offset: 0, End: 4}},
		Phase: PhaseActive, CommitState: CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewFileCheckpointReference(record)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}
