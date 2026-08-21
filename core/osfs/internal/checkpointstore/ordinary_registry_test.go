package checkpointstore

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOperationRegistryActiveLastLookupLeaseAndTerminalExclusion(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x21)

	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("begin active = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	if retry, err := registry.LookupActive(fixture.key); err != nil || retry.State() != ActiveLookupNeedsAttention {
		t.Fatalf("pre-index retry = %v, %v", retry.State(), err)
	}

	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind reservation = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	record := fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim)
	lease, err := admission.Create(record, claim)
	if err != nil {
		t.Fatal(err)
	}
	if lookup, err := registry.LookupActive(fixture.key); err != nil || lookup.State() != ActiveLookupAlreadyRunning {
		t.Fatalf("held lookup = %v, %v", lookup.State(), err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	reopen, err := registry.LookupActive(fixture.key)
	if err != nil || reopen.State() != ActiveLookupReopenable {
		t.Fatalf("released lookup = %v, %v", reopen.State(), err)
	}
	reopenedLease := reopen.TakeLease()
	if reopenedLease == nil || reopenedLease.Record().Lease() != checkpointmodel.OrdinaryLeaseHeld {
		t.Fatal("reopen did not transfer exact held lease")
	}
	if proof := reopen.RecoveryProof(); !proof.Valid() || proof.Claim().Generation != claim.Generation+1 ||
		len(proof.PersistentIdentity()) != 0 {
		t.Fatalf("recovery proof = %+v", proof)
	}
	previous := reopenedLease.Record()
	next, err := checkpointmodel.NextOrdinaryOperationRecord(previous, checkpointmodel.NextOrdinaryOperationRecordSpec{
		Lifecycle:    checkpointmodel.OrdinaryOperationCompleted,
		Lease:        checkpointmodel.OrdinaryLeaseHeld,
		ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	replaceErr := reopenedLease.Replace(previous, next)
	if err != nil || replaceErr != nil {
		t.Fatalf("complete = reduce %v, replace %v", err, replaceErr)
	}
	if err := reopenedLease.Close(); err != nil {
		t.Fatal(err)
	}
	if terminal, err := registry.LookupActive(fixture.key); err != nil || terminal.State() != ActiveLookupNone {
		t.Fatalf("terminal lookup = %v, %v", terminal.State(), err)
	}
	page, err := registry.PageOperations(OperationPageCursor{}, 1)
	if err != nil || len(page.Records()) != 1 || page.Records()[0].Lifecycle() != checkpointmodel.OrdinaryOperationCompleted {
		t.Fatalf("terminal page = %+v, %v", page.Records(), err)
	}
}

func TestDeleteTerminalDoesNotRecreateRowAfterUnlinkSyncFailure(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x23)
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("begin active = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind reservation = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := admission.Create(
		fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	previous := lease.Record()
	completed, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle:    checkpointmodel.OrdinaryOperationCompleted,
			Lease:        checkpointmodel.OrdinaryLeaseHeld,
			ClosedReason: checkpointmodel.OrdinaryReasonNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Replace(previous, completed); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("operation parent sync failed")
	registry.operations = syncFailureDirectory{
		Directory: registry.operations,
		err:       failure,
	}
	if err := lease.DeleteTerminal(); !errors.Is(err, failure) {
		t.Fatalf("delete terminal error = %v", err)
	}
	if !lease.Deleted() || lease.Record().Valid() {
		t.Fatal("unlinked lease retained authority to recreate its terminal row")
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("deleted lease close = %v", err)
	}
}

type syncFailureDirectory struct {
	outputcap.Directory
	err error
}

func (directory syncFailureDirectory) Sync() error { return directory.err }

func TestAcquireOperationLeaseIsExplicitAndRestoresHeldState(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x24)
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("begin active = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	_ = handle.Close()
	lease, err := admission.Create(fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	acquired, err := registry.AcquireOperationLease(fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	if acquired.Record().Lease() != checkpointmodel.OrdinaryLeaseHeld {
		t.Fatalf("lease = %d", acquired.Record().Lease())
	}
	if err := acquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationRegistryTerminalTransitionRetiresExactPostIndexCandidate(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x25)
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("begin active = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind reservation = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	// Recreate the crash cut where the active index is durable but retiring its
	// exact candidate did not complete.
	candidate := admission.candidate
	lease, err := admission.Create(fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim)
	if err != nil {
		t.Fatal(err)
	}
	candidateDirectory, err := openOrCreateDirectory(registry.candidates, activeKeyName(fixture.key))
	if err != nil {
		t.Fatal(err)
	}
	encodedCandidate, err := checkpointmodel.EncodeOrdinaryAdmissionCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	installErr := InstallCreate(candidateDirectory, ordinaryAdmissionCandidateFile, encodedCandidate)
	if closeErr := candidateDirectory.Close(); installErr != nil || closeErr != nil {
		t.Fatalf("restore post-index candidate = install %v, close %v", installErr, closeErr)
	}

	previous := lease.Record()
	next, err := checkpointmodel.NextOrdinaryOperationRecord(previous, checkpointmodel.NextOrdinaryOperationRecordSpec{
		Lifecycle: checkpointmodel.OrdinaryOperationCompleted, Lease: checkpointmodel.OrdinaryLeaseHeld,
		ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Replace(previous, next); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if terminal, err := registry.LookupActive(fixture.key); err != nil || terminal.State() != ActiveLookupNone {
		t.Fatalf("terminal lookup after post-index crash = %v, %v", terminal.State(), err)
	}
}

func TestOperationRegistryTerminalTransitionPreservesUnknownCandidateNamespace(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x26)
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("begin active = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind reservation = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := admission.Create(fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim)
	if err != nil {
		t.Fatal(err)
	}
	candidateDirectory, err := openOrCreateDirectory(registry.candidates, activeKeyName(fixture.key))
	if err != nil {
		t.Fatal(err)
	}
	opaque, err := candidateDirectory.CreateFile("opaque", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(opaque.Close(), candidateDirectory.Close()); err != nil {
		t.Fatal(err)
	}
	previous := lease.Record()
	next, err := checkpointmodel.NextOrdinaryOperationRecord(previous, checkpointmodel.NextOrdinaryOperationRecordSpec{
		Lifecycle: checkpointmodel.OrdinaryOperationCompleted, Lease: checkpointmodel.OrdinaryLeaseHeld,
		ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Replace(previous, next); err == nil {
		t.Fatal("terminal transition deleted or ignored an unknown candidate namespace")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if lookup, err := registry.LookupActive(fixture.key); err != nil || lookup.State() != ActiveLookupAmbiguous {
		t.Fatalf("terminal unknown lookup = %v, %v", lookup.State(), err)
	}
	candidateDirectory, err = openExistingDirectory(registry.candidates, activeKeyName(fixture.key))
	if err != nil {
		t.Fatal(err)
	}
	if kind, exact, err := candidateDirectory.ClassifyExactEntry("opaque"); err != nil ||
		!exact || kind != outputcap.EntryRegularFile {
		t.Fatalf("preserved opaque entry = %v, exact %v, %v", kind, exact, err)
	}
	if err := candidateDirectory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationRegistryPreservesUnknownCandidateNamespaceAsAmbiguous(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x26)
	candidateDirectory, err := openOrCreateDirectory(registry.candidates, activeKeyName(fixture.key))
	if err != nil {
		t.Fatal(err)
	}
	opaque, err := candidateDirectory.CreateFile("opaque", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(opaque.Close(), candidateDirectory.Close()); err != nil {
		t.Fatal(err)
	}

	lookup, err := registry.LookupActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupAmbiguous {
		t.Fatalf("unknown candidate lookup = %v, %v", lookup.State(), err)
	}
	candidateDirectory, err = openExistingDirectory(registry.candidates, activeKeyName(fixture.key))
	if err != nil {
		t.Fatal(err)
	}
	if kind, exact, err := candidateDirectory.ClassifyExactEntry("opaque"); err != nil ||
		!exact || kind != outputcap.EntryRegularFile {
		t.Fatalf("preserved opaque entry = %v, exact %v, %v", kind, exact, err)
	}
	if err := candidateDirectory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationRegistryReopensCrashStaleHeldLeaseAndAuthenticatesClaim(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x2a)
	admission, _, err := registry.BeginActive(fixture.key)
	if err != nil || admission.PrepareCandidate(fixture.intent.OperationID()) != nil {
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := admission.Create(fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim)
	if err != nil {
		t.Fatal(err)
	}
	// A process crash releases the native lock without rewriting its durable row.
	if err := closeLock(lease.lock); err != nil {
		t.Fatal(err)
	}
	lease.lock = nil

	reopened, err := registry.LookupActive(fixture.key)
	if err != nil || reopened.State() != ActiveLookupReopenable ||
		reopened.Record().LifecycleGeneration() != 2 || !reopened.RecoveryProof().Valid() {
		t.Fatalf("crash reopen = state %v generation %d proof %v err %v",
			reopened.State(), reopened.Record().LifecycleGeneration(), reopened.RecoveryProof().Valid(), err)
	}
	if err := reopened.TakeLease().Close(); err != nil {
		t.Fatal(err)
	}

	claimRecord, claimName, err := registry.readClaimByToken([32]byte(reopened.RecoveryProof().Claim().Token),
		reopened.RecoveryProof().Claim().Generation)
	if err != nil {
		t.Fatal(err)
	}
	claimBytes, _ := checkpointmodel.EncodeReservationClaimRecord(claimRecord)
	claimBytes[len(claimBytes)-1] ^= 0xff
	registry.claims.(*memoryDirectory).files[claimName].bytes = claimBytes
	if lookup, err := registry.LookupActive(fixture.key); err != nil || lookup.State() != ActiveLookupAmbiguous {
		t.Fatalf("tampered reverse claim = %v, %v", lookup.State(), err)
	}
}

func TestOperationRegistryClaimSerializesDifferentActiveKeysAndPreservesUnknown(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	first := ordinaryRegistryFixture(t, 0x31)
	second := ordinaryRegistryFixture(t, 0x41)
	second.claimSpec.CanonicalNameKey = first.claimSpec.CanonicalNameKey
	second.claimSpec.RequestedName = first.claimSpec.RequestedName
	second.claimSpec.LogicalReservedName = first.claimSpec.LogicalReservedName
	second.claimSpec.PhysicalName = first.claimSpec.PhysicalName

	handle, outcome, err := registry.BeginReservation(first.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("first claim = %v, %v", outcome, err)
	}
	if outcome, err := handle.BindReservation(first.reservation); err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind first = %v, %v", outcome, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	foreign, outcome, err := registry.BeginReservation(second.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCollision || foreign != nil {
		t.Fatalf("foreign claim = %T, %v, %v", foreign, outcome, err)
	}

	registry.claims.(*memoryDirectory).files["foreign"] = &memoryFileData{bytes: []byte("foreign")}
	page, err := registry.PageReservationClaims(ReservationClaimPageCursor{}, 4)
	if err != nil || !page.Unknown() || len(page.Records()) != 1 {
		t.Fatalf("claim page = %+v unknown=%t, %v", page.Records(), page.Unknown(), err)
	}
	if _, err := ReadFile(registry.claims, "foreign"); err != nil {
		t.Fatalf("unknown claim mutated: %v", err)
	}
}

func TestOperationRegistryPagesBeyondFirstBoundedWindow(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for index := byte(1); index <= 3; index++ {
		fixture := ordinaryRegistryFixture(t, 0x60+index)
		claim, _ := checkpointmodel.NewReservationClaimLocator([32]byte{index}, 1)
		record := fixture.record(t, checkpointmodel.OrdinaryLeaseReleased, destinationauthority.ReservationClaim{
			Token: destinationauthority.ReservationClaimToken(claim.Token()), Generation: claim.Generation() - 1,
		})
		encoded, _ := checkpointmodel.EncodeOrdinaryOperationRecord(record)
		directory, _ := openOrCreateDirectory(registry.operations, operationNamespaceName(record.OperationID()))
		if err := InstallCreate(directory, ordinaryOperationRecordFile, encoded); err != nil {
			t.Fatal(err)
		}
		_ = directory.Close()
	}
	var operations []receivecontract.OperationID
	cursor := OperationPageCursor{}
	for {
		page, err := registry.PageOperations(cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records() {
			operations = append(operations, record.OperationID())
		}
		if page.Next().IsZero() {
			break
		}
		cursor = page.Next()
	}
	if len(operations) != 3 {
		t.Fatalf("paged operations = %d", len(operations))
	}
}

func TestOperationRegistryAdmissionCandidateRollbackAndAttentionAreExact(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	fixture := ordinaryRegistryFixture(t, 0x75)
	admission, _, err := registry.BeginActive(fixture.key)
	if err != nil || admission.PrepareCandidate(fixture.intent.OperationID()) != nil {
		t.Fatal(err)
	}
	if err := admission.RollbackCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := admission.Close(); err != nil {
		t.Fatal(err)
	}
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		t.Fatalf("clean rollback lookup = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		t.Fatal(err)
	}
	if err := admission.RequireAttention(); err != nil {
		t.Fatal(err)
	}
	if err := admission.Close(); err != nil {
		t.Fatal(err)
	}
	if lookup, err := registry.LookupActive(fixture.key); err != nil || lookup.State() != ActiveLookupNeedsAttention {
		t.Fatalf("attention lookup = %v, %v", lookup.State(), err)
	}
}

func TestLiveCleanupJournalBoundedSnapshotAndCompareGeneration(t *testing.T) {
	control := newMemoryDirectory()
	journal, err := OpenLiveCleanupJournal(control)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	first := liveCleanupTicketFixture(t, 0x51)
	second := liveCleanupTicketFixture(t, 0x52)
	if err := journal.Create(first); err != nil {
		t.Fatal(err)
	}
	if err := journal.Create(second); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := journal.Snapshot(1); err != nil || snapshot.State != destinationauthority.LiveCleanupScanOverflow || len(snapshot.Tickets) != 0 {
		t.Fatalf("overflow snapshot = %+v, %v", snapshot, err)
	}
	if snapshot, err := journal.Snapshot(2); err != nil || snapshot.State != destinationauthority.LiveCleanupScanComplete || len(snapshot.Tickets) != 2 {
		t.Fatalf("complete snapshot = %+v, %v", snapshot, err)
	}
	if syncs := directorySyncCalls(journal.tickets.(*memoryDirectory)); syncs < 2 {
		t.Fatalf("ticket creates were not directory-synced: %d", syncs)
	}
	created, _ := checkpointmodel.ReduceLiveCleanupTicket(first, checkpointmodel.LiveCleanupRecordStageCreated)
	if err := journal.Replace(first, created); err != nil {
		t.Fatal(err)
	}
	if err := journal.Replace(first, created); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("stale replace = %v", err)
	}
	if err := journal.Delete(created); err != nil {
		t.Fatal(err)
	}
	if err := journal.Delete(created); err != nil {
		t.Fatalf("idempotent delete = %v", err)
	}
	if _, err := ReadFile(journal.tickets, liveCleanupTicketName(created)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted ticket = %v", err)
	}

	journal.tickets.(*memoryDirectory).files["opaque"] = &memoryFileData{bytes: []byte("opaque")}
	if snapshot, err := journal.Snapshot(2); err != nil || snapshot.State != destinationauthority.LiveCleanupScanUnknown {
		t.Fatalf("unknown snapshot = %+v, %v", snapshot, err)
	}
	if _, err := ReadFile(journal.tickets, "opaque"); err != nil {
		t.Fatalf("unknown ticket mutated: %v", err)
	}
}

type ordinaryRegistryTestFixture struct {
	intent      transfer.ReceiveIntent
	key         checkpointmodel.ActiveOperationKey
	reservation receivecontract.DestinationReservation
	claimSpec   destinationauthority.ReservationClaimSpec
}

func ordinaryRegistryFixture(t *testing.T, fill byte) ordinaryRegistryTestFixture {
	t.Helper()
	var share catalog.ShareInstance
	var rootID catalog.DirectoryID
	var fileID catalog.FileID
	share[0], rootID[0], fileID[0] = fill, fill+1, fill+2
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, _ := transfer.NewSelectionSpec(share, rootID, rules)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, "download.txt", "download.txt")
	if err != nil {
		t.Fatal(err)
	}
	operationID, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{fill + 3}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{fill + 4}, receivecontract.StableIdentityBytes))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{fill + 5}, receivecontract.AuthorityRefBytes))
	reservation, err := receivecontract.NewNativeNamedEntryReservation(operationID, reservationID, artifact, authority, "download.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	if err != nil {
		t.Fatal(err)
	}
	return ordinaryRegistryTestFixture{
		intent: intent, key: key, reservation: reservation,
		claimSpec: destinationauthority.ReservationClaimSpec{
			CanonicalNameKey: reservation.PhysicalName(), OperationID: intent.OperationID(), ReservationID: reservation.ID(),
			EntryKind: reservation.EntryKind(), RequestedName: reservation.RequestedName(),
			LogicalReservedName: reservation.LogicalReservedName(), PhysicalName: reservation.PhysicalName(),
			CollisionIndex: reservation.CollisionIndex(),
		},
	}
}

func (fixture ordinaryRegistryTestFixture) record(
	t *testing.T,
	lease checkpointmodel.OrdinaryLeaseState,
	claim destinationauthority.ReservationClaim,
) checkpointmodel.OrdinaryOperationRecord {
	t.Helper()
	record, err := checkpointmodel.NewOrdinaryOperationRecord(checkpointmodel.OrdinaryOperationRecordSpec{
		ActiveKey: fixture.key, Intent: fixture.intent, LifecycleGeneration: 1,
		ReservationClaim: mustReservationClaimLocator(t, claim),
		Lifecycle:        checkpointmodel.OrdinaryOperationActive, Lease: lease,
		ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func mustReservationClaimLocator(t *testing.T, claim destinationauthority.ReservationClaim) checkpointmodel.ReservationClaimLocator {
	t.Helper()
	locator, err := checkpointmodel.NewReservationClaimLocator([32]byte(claim.Token), claim.Generation+1)
	if err != nil {
		t.Fatal(err)
	}
	return locator
}

func liveCleanupTicketFixture(t *testing.T, fill byte) checkpointmodel.LiveCleanupTicket {
	t.Helper()
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: bytes.Repeat([]byte{fill}, checkpointmodel.LiveCleanupNonceBytesV1), ExactSize: uint64(fill),
		Profile: checkpointmodel.LiveCleanupWindowsNTFSV1, Generation: 1,
		State: checkpointmodel.LiveCleanupTicketCommitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
