package checkpointstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCompatibleLookupExposesAmbiguityAndRetainsUnknownOwnership(t *testing.T) {
	_, namespace, firstLease, firstRepository, _, first := openRepositoryFixture(t, 0xd5)
	defer namespace.Close()
	if err := firstRepository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstLease.Close(); err != nil {
		t.Fatal(err)
	}

	second := compatibleOperationFixture(t, first, 0xe1)
	secondLease, err := namespace.AcquireOperation(
		second.intent.OperationID(), second.intent.Digest(), second.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	secondRepository, err := secondLease.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRepository.InstallOperation(second.operation, second.binding); err != nil {
		t.Fatal(err)
	}
	frozen, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: second.intent.OperationID(), ReceiveIntent: second.intent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRepository.CreateLifecycleState(frozen); err != nil {
		t.Fatal(err)
	}
	if err := secondLease.RegisterLookup(second.operation); err != nil {
		t.Fatal(err)
	}
	if err := secondRepository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secondLease.Close(); err != nil {
		t.Fatal(err)
	}

	key := first.operation.ReopenKey().CompatibleKey()
	lookup, err := namespace.LookupCompatible(key)
	if err != nil || lookup.OwnershipUncertain() || len(lookup.Operations()) != 2 {
		t.Fatalf(
			"ambiguous lookup = (%d operations, uncertain=%t, %v)",
			len(lookup.Operations()), lookup.OwnershipUncertain(), err,
		)
	}
	for _, operation := range lookup.Operations() {
		if _, err := operation.VerifyIntent(transfer.DecodeReceiveIntent); err != nil {
			t.Fatalf("ambiguous candidate did not survive fresh-process decode: %v", err)
		}
	}

	lookupRoot, ok := namespace.lookup.(*memoryDirectory)
	if !ok {
		t.Fatalf("lookup root = %T", namespace.lookup)
	}
	keyDirectory := lookupRoot.dirsForTest(t, bytesToHex(key.Bytes()))
	if err := InstallCreate(keyDirectory, "not-an-operation", []byte{1}); err != nil {
		t.Fatal(err)
	}
	missingOperation, err := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{0xf1}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallCreate(
		keyDirectory, operationNamespaceName(missingOperation), bytes.Repeat([]byte{0xf2}, 32),
	); err != nil {
		t.Fatal(err)
	}
	lookup, err = namespace.LookupCompatible(key)
	if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 2 {
		t.Fatalf(
			"uncertain ambiguous lookup = (%d operations, uncertain=%t, %v)",
			len(lookup.Operations()), lookup.OwnershipUncertain(), err,
		)
	}
}

func compatibleOperationFixture(t *testing.T, base operationFixture, seed byte) operationFixture {
	t.Helper()
	baseReservation, direct := base.intent.MaterializationPlan().DestinationReservation()
	if !direct {
		t.Fatal("base operation lost its DirectTree reservation")
	}
	operationID, err := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{seed}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{seed + 1}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact := base.intent.ArtifactSpec()
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operationID, reservationID, artifact, baseReservation.AuthorityRef(),
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(base.intent.SelectionSpec(), artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkpointmodel.NewCLICompatibleOperationKey(
		intent.SelectionSpec(), artifact, baseReservation.AuthorityRef(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if key != base.operation.ReopenKey().CompatibleKey() {
		t.Fatal("compatible operation fixture changed the lookup identity")
	}
	reopen, err := checkpointmodel.CLIReopenKey(key)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := checkpointmodel.NewReceiveOperation(intent, reopen)
	if err != nil {
		t.Fatal(err)
	}
	return operationFixture{intent: intent, operation: operation, binding: reservation.CanonicalBytes()}
}

func TestAggregateRepositoryRejectsTamperedReceiptsAndLifecycleImages(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x41)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	reference := installAggregateCheckpoint(t, &repository, ownership, fixture, 0x51)
	evidence, err := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptTreeCompletion, OperationID: fixture.intent.OperationID(),
		ReceiveIntent: fixture.intent.Digest(), ReservationDigest: fixture.intent.BindingDigest(),
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference},
		EvidenceDigest: evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(receipt); err != nil {
		t.Fatal(err)
	}

	receipts, ok := repository.receipts.(*memoryDirectory)
	if !ok {
		t.Fatalf("receipt directory = %T", repository.receipts)
	}
	receiptName := hex.EncodeToString(receipt.Digest().Bytes())
	receiptFile := receipts.files[receiptName]
	receiptFile.mu.Lock()
	originalReceipt := bytes.Clone(receiptFile.bytes)
	receiptFile.bytes[0] ^= 1
	receiptFile.mu.Unlock()
	if _, err := repository.ReadReceipt(receipt.Digest()); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("tampered receipt error = %v", err)
	}
	receiptFile.mu.Lock()
	receiptFile.bytes = originalReceipt
	receiptFile.mu.Unlock()

	foreignDigest, err := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0x62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallCreate(
		receipts, hex.EncodeToString(foreignDigest.Bytes()), receipt.CanonicalBytes(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReadReceipt(foreignDigest); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("digest-substituted receipt error = %v", err)
	}

	lifecycleFile := receipts.files[lifecycleStateFile]
	lifecycleFile.mu.Lock()
	originalLifecycle := bytes.Clone(lifecycleFile.bytes)
	lifecycleFile.bytes[len(lifecycleFile.bytes)-1] ^= 1
	lifecycleFile.mu.Unlock()
	if _, err := repository.ReadLifecycleState(); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("tampered lifecycle error = %v", err)
	}
	lifecycleFile.mu.Lock()
	lifecycleFile.bytes = originalLifecycle
	lifecycleFile.mu.Unlock()

	missingReceiptState, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 4, Phase: checkpointmodel.LifecyclePublished,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference},
		ReceiptDigest:  foreignDigest, SuccessCount: 1,
		CleanupState: checkpointmodel.OwnedCleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.verifyLifecycleAuthorities(missingReceiptState); errorCode(err) != ErrorCorruptRecord {
		// A digest-substituted image exists but cannot authenticate the requested receipt.
		t.Fatalf("unauthenticated lifecycle receipt error = %v", err)
	}
	missingDigest, err := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	missingReceiptState, err = checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 4, Phase: checkpointmodel.LifecyclePublished,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference},
		ReceiptDigest:  missingDigest, SuccessCount: 1,
		CleanupState: checkpointmodel.OwnedCleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.verifyLifecycleAuthorities(missingReceiptState); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("missing lifecycle receipt error = %v", err)
	}
	if lease.Binding().OperationID() != fixture.intent.OperationID() {
		t.Fatal("operation lease lost its immutable binding")
	}

	var nilRepository *Repository
	if _, err := nilRepository.ReadReceipt(checkpointmodel.AggregateDigest{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil receipt repository error = %v", err)
	}
	if _, err := nilRepository.ReadLifecycleState(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil lifecycle repository error = %v", err)
	}
}

func installAggregateCheckpoint(
	t *testing.T,
	repository *Repository,
	ownership checkpointmodel.Ownership,
	fixture operationFixture,
	seed byte,
) checkpointmodel.FileCheckpointReference {
	t.Helper()
	candidate := checkpointRecordFixture(t, ownership, fixture, seed)
	initial, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(candidate); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(candidate, initial); err != nil {
		t.Fatal(err)
	}
	nextCandidate, err := checkpointmodel.AdvanceGeneration(
		initial, []checkpointmodel.Range{{Offset: 0, End: initial.ExactSize()}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := checkpointmodel.Promote(
		nextCandidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initial, nextCandidate); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(nextCandidate, verified); err != nil {
		t.Fatal(err)
	}
	reference, err := checkpointmodel.NewFileCheckpointReference(verified)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestRecoveryLocationsAndPinnedNamespaceRejectNonCanonicalAuthority(t *testing.T) {
	_, namespace, lease, repository, ownership, _ := openRepositoryFixture(t, 0x71)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []RecoveryArtifactKind{RecoveryStage, RecoveryAnchor} {
		shard, name, err := RecoveryArtifactLocation(object, kind)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := ParseRecoveryArtifactLocation(shard, name, kind)
		if err != nil || restored != object {
			t.Fatalf("artifact location %d = (%x, %v)", kind, restored, err)
		}
		if _, err := ParseRecoveryArtifactLocation("zz", name, kind); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("unsafe artifact shard error = %v", err)
		}
	}
	if _, _, err := RecoveryArtifactLocation(checkpointmodel.ObjectID{}, RecoveryStage); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("zero artifact location error = %v", err)
	}
	if _, _, err := RecoveryArtifactLocation(object, 0); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("open artifact kind error = %v", err)
	}

	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatal(err)
	}
	shard, name := RecordLocation(recordID)
	restored, err := ParseRecordLocation(shard, name)
	if err != nil || restored != recordID {
		t.Fatalf("record location = (%x, %v)", restored, err)
	}
	if _, err := ParseRecordLocation(shard, name+".foreign"); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("foreign record suffix error = %v", err)
	}

	if _, known := CheckpointRootEntryKind(OwnershipDirectory); !known || CheckpointRootEntryLimit() == 0 {
		t.Fatal("checkpoint root schema lost its closed ownership entry")
	}
	if _, known := CheckpointRootEntryKind("foreign"); known {
		t.Fatal("foreign checkpoint root entry became known")
	}
	if _, known := OperationEntryKind(OperationFile); !known || OperationEntryLimit() == 0 {
		t.Fatal("operation schema lost its immutable operation record")
	}
	if _, known := OperationEntryKind("foreign"); known {
		t.Fatal("foreign operation entry became known")
	}

	pinned, err := AdoptPinnedNamespace(
		newMemoryDirectory(), newMemoryDirectory(), newMemoryDirectory(), newMemoryDirectory(), ownership,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := AdoptPinnedNamespace(nil, newMemoryDirectory(), newMemoryDirectory(), newMemoryDirectory(), ownership); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil pinned checkpoint root error = %v", err)
	}
	if ErrorCodeFor(checkpointmodel.ErrRecordBinding) != ErrorUnsafeInstall {
		t.Fatal("record binding error escaped the closed storage vocabulary")
	}
}

func TestRepositoryCleanupRemovesOnlyTheExactCheckpointImage(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x81)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	record := checkpointRecordFixture(t, ownership, fixture, 0x82)
	if err := repository.Create(record); err != nil {
		t.Fatal(err)
	}
	if err := repository.Remove(record); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Reopen(record.RecordID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed checkpoint reopen error = %v", err)
	}
	if err := repository.Remove(record); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("repeated checkpoint removal error = %v", err)
	}

	foreign := checkpointRecordFixture(t, ownership, fixture, 0x83)
	if err := repository.Remove(foreign); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("foreign checkpoint removal error = %v", err)
	}
	var nilRepository *Repository
	if err := nilRepository.Remove(record); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil checkpoint removal error = %v", err)
	}
}
