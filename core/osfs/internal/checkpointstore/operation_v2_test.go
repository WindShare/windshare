package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func TestAdmittedDirectoryRepositoryBindsOneCatalogIdentityAndGeneration(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0xb1)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()
	scope, err := transfer.NewDirectoryAdmissionScope(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{0xb2}, catalog.IdentityBytes))
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0xb3}, sha256.Size), scope,
		transfer.MaterializationDirectory{
			DirectoryID: fixture.intent.SyntheticRoot(), Generation: generation,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	owned, _ := transfer.OwnedObjectIDFromBytes(bytes.Repeat([]byte{0xb4}, transfer.OwnedObjectIdentityBytes))
	record, err := checkpointmodel.NewAdmittedDirectory(fixture.intent, admission, owned)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallAdmittedDirectory(record); err != nil {
		t.Fatal(err)
	}
	restored, err := repository.ReadAdmittedDirectory(admission.DirectoryID())
	if err != nil || restored.Generation() != generation || restored.OwnedObjectID() != owned {
		t.Fatalf("restored admission = (%+v, %v)", restored, err)
	}

	otherGeneration, _ := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{0xb5}, catalog.IdentityBytes))
	otherAdmission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0xb6}, sha256.Size), scope,
		transfer.MaterializationDirectory{
			DirectoryID: fixture.intent.SyntheticRoot(), Generation: otherGeneration,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := checkpointmodel.NewAdmittedDirectory(fixture.intent, otherAdmission, owned)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallAdmittedDirectory(conflict); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("generation conflict error = %v", err)
	}
}

func TestOperationLookupFreshProcessVerifiesIntentAndReservation(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0xc1)
	defer namespace.Close()
	defer lease.Close()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	lookup, err := namespace.LookupCompatible(fixture.operation.ReopenKey().CompatibleKey())
	if err != nil || lookup.OwnershipUncertain() || len(lookup.Operations()) != 1 {
		t.Fatalf("fresh lookup = (%d, uncertain=%t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
	}
	operation := lookup.Operations()[0]
	decoded, err := operation.VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil || !decoded.EqualCanonical(fixture.intent) {
		t.Fatalf("fresh-process intent verification = %v", err)
	}

	lookupRoot, ok := namespace.lookup.(*memoryDirectory)
	if !ok {
		t.Fatalf("lookup root = %T", namespace.lookup)
	}
	keyDirectory := lookupRoot.dirsForTest(
		t, bytesToHex(fixture.operation.ReopenKey().CompatibleKey().Bytes()),
	)
	index := keyDirectory.files[operationNamespaceName(fixture.intent.OperationID())]
	index.mu.Lock()
	index.bytes[0] ^= 1
	index.mu.Unlock()
	lookup, err = namespace.LookupCompatible(fixture.operation.ReopenKey().CompatibleKey())
	if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
		t.Fatalf("corrupt lookup = (%d, uncertain=%t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
	}
}

func TestOperationLookupBusyCandidateIsUncertainAndTerminalStateIsExcluded(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0xc7)
	defer namespace.Close()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	lookup, err := namespace.LookupCompatible(fixture.operation.ReopenKey().CompatibleKey())
	if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
		t.Fatalf("busy lookup = (%d, uncertain=%t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	lease, err = namespace.AcquireOperation(
		fixture.intent.OperationID(), fixture.intent.Digest(), fixture.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = lease.OpenExistingRepository()
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := repository.ReadLifecycleState()
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0xc8}, 32))
	cleanup, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptCleanup, OperationID: fixture.intent.OperationID(),
		ReceiveIntent: fixture.intent.Digest(), ReservationDigest: fixture.intent.BindingDigest(),
		EvidenceDigest: evidence, CleanupGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(cleanup); err != nil {
		t.Fatal(err)
	}
	discarded, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 2, Phase: checkpointmodel.LifecycleDiscarded,
		ReceiptDigest: cleanup.Digest(), CleanupState: checkpointmodel.OwnedCleanupClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceLifecycleState(frozen, discarded); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	lookup, err = namespace.LookupCompatible(fixture.operation.ReopenKey().CompatibleKey())
	if err != nil || lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
		t.Fatalf("terminal lookup = (%d, uncertain=%t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
	}
}

func TestAggregateLifecycleReferencesOnlyVerifiedCheckpointGenerations(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xd1)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	initialCandidate := checkpointRecordFixture(t, ownership, fixture, 0xd2)
	initial, err := checkpointmodel.PromoteInitialCandidate(initialCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(initialCandidate); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initialCandidate, initial); err != nil {
		t.Fatal(err)
	}
	candidate, err := checkpointmodel.AdvanceGeneration(
		initial, []checkpointmodel.Range{{Offset: 0, End: initial.ExactSize()}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := checkpointmodel.Promote(
		candidate, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initial, candidate); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(candidate, verified); err != nil {
		t.Fatal(err)
	}
	reference, err := checkpointmodel.NewFileCheckpointReference(verified)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0xe1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptTreeCompletion,
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		ReservationDigest: fixture.intent.BindingDigest(),
		CheckpointRefs:    []checkpointmodel.FileCheckpointReference{reference},
		EvidenceDigest:    evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if restored, err := repository.ReadReceipt(receipt.Digest()); err != nil ||
		restored.Digest() != receipt.Digest() {
		t.Fatalf("receipt read = (%x, %v)", restored.Digest(), err)
	}

	frozen, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateLifecycleState(frozen); err != nil {
		t.Fatal(err)
	}
	receiving := lifecycleFixtureState(t, fixture, 2, checkpointmodel.LifecycleReceiving, reference, checkpointmodel.AggregateDigest{})
	if err := repository.ReplaceLifecycleState(frozen, receiving); err != nil {
		t.Fatal(err)
	}
	finalizing := lifecycleFixtureState(t, fixture, 3, checkpointmodel.LifecycleFinalizingTree, reference, checkpointmodel.AggregateDigest{})
	if err := repository.ReplaceLifecycleState(receiving, finalizing); err != nil {
		t.Fatal(err)
	}
	published, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 4, Phase: checkpointmodel.LifecyclePublished,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference},
		ReceiptDigest:  receipt.Digest(), SuccessCount: 1,
		CleanupState: checkpointmodel.OwnedCleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceLifecycleState(finalizing, published); err != nil {
		t.Fatal(err)
	}
	if restored, err := repository.ReadLifecycleState(); err != nil ||
		!sameLifecycleState(restored, published) {
		t.Fatalf("lifecycle read = (%+v, %v)", restored, err)
	}

	foreignReference, err := checkpointmodel.FileCheckpointReferenceFromIdentity(
		reference.RecordID(), reference.CheckpointGeneration()+1,
	)
	if err != nil {
		t.Fatal(err)
	}
	foreignReceipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind:        checkpointmodel.ReceiptTreeCompletion,
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		ReservationDigest: fixture.intent.BindingDigest(),
		CheckpointRefs:    []checkpointmodel.FileCheckpointReference{foreignReference},
		EvidenceDigest:    evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(foreignReceipt); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("foreign generation error = %v", err)
	}
	quarantined, err := checkpointmodel.AdvanceState(
		verified, verified.StateGeneration()+1,
		checkpointmodel.PhaseQuarantined, checkpointmodel.CommitQuarantined,
		checkpointmodel.QuarantineAnchorUnsafe, checkpointmodel.QuarantineOriginWitnessed, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(verified, quarantined); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReadReceipt(receipt.Digest()); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("quarantined checkpoint reference error = %v", err)
	}
}

func lifecycleFixtureState(
	t *testing.T,
	fixture operationFixture,
	generation uint64,
	phase checkpointmodel.LifecyclePhase,
	reference checkpointmodel.FileCheckpointReference,
	receipt checkpointmodel.AggregateDigest,
) checkpointmodel.ReceiveLifecycleState {
	t.Helper()
	record, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: generation, Phase: phase,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference},
		ReceiptDigest:  receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func bytesToHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	encoded := make([]byte, len(value)*2)
	for index, current := range value {
		encoded[index*2] = alphabet[current>>4]
		encoded[index*2+1] = alphabet[current&0x0f]
	}
	return string(encoded)
}
