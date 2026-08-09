package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCertifiedLayoutMatchesTheDurableOwnershipNamespace(t *testing.T) {
	if ControlDirectory+"/"+CheckpointDirectory != checkpointmodel.NamespaceName {
		t.Fatalf("store namespace %q diverged from ownership namespace %q",
			ControlDirectory+"/"+CheckpointDirectory, checkpointmodel.NamespaceName)
	}
}

func TestAttentionReferencesAreStableAndDoNotExposeInternalNames(t *testing.T) {
	first := newAttention(AttentionUnknownEntry, "0a", "private-name")
	repeated := newAttention(AttentionUnknownEntry, "0a", "private-name")
	different := newAttention(AttentionUnknownEntry, "0a", "other-name")
	if first.Reference() != repeated.Reference() || first.Reference() == different.Reference() ||
		len(first.Reference()) != sha256.Size*2 || strings.Contains(first.Reference(), "private-name") ||
		!first.Code().Valid() || AttentionCode("future-attention").Valid() {
		t.Fatalf("attention reference is not an opaque stable correlation token: %q", first.Reference())
	}
}

func TestRepositoryScanBudgetSeparatesRecordsFromPreservedAuxiliaryState(t *testing.T) {
	budget := newRepositoryScanBudget(1, 1)
	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0x2a}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	shard, recordName := recordLocation(recordID)
	if limit, err := budget.namesLimit(); err != nil || limit != 3 {
		t.Fatalf("initial names limit = %d, %v", limit, err)
	}
	if err := budget.observe(shard, recordName); err != nil {
		t.Fatal(err)
	}
	if err := budget.observe(shard, "opaque-entry"); err != nil {
		t.Fatal(err)
	}
	if limit, err := budget.namesLimit(); err != nil || limit != 1 {
		t.Fatalf("exhausted names limit = %d, %v", limit, err)
	}
	if err := budget.observe(shard, recordName); !errors.Is(err, checkpointmodel.ErrRecordRecovery) {
		t.Fatalf("record overflow = %v", err)
	}
	if err := budget.observe(shard, ".candidate-foreign"); !errors.Is(err, checkpointmodel.ErrRecordRecovery) {
		t.Fatalf("auxiliary overflow = %v", err)
	}
}

func TestRepositoryReconcileAppliesOneEntryBudgetAcrossAllShards(t *testing.T) {
	_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x2b)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	for _, shardName := range []string{"00", "01"} {
		shard, err := OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, shard, "opaque-entry", []byte("preserve"))
		if err := shard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	budget := newRepositoryScanBudget(1, 1)
	_, err := repository.reconcile(func(checkpointmodel.Record) (bool, error) {
		t.Fatal("opaque state unexpectedly requested a checkpoint witness")
		return false, nil
	}, &budget)
	if errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("global auxiliary overflow = %v", err)
	}
	for _, shardName := range []string{"00", "01"} {
		shard, openErr := OpenShard(repository.records, shardName, false)
		if openErr != nil {
			t.Fatal(openErr)
		}
		encoded, readErr := ReadFile(shard, "opaque-entry")
		closeErr := shard.Close()
		if readErr != nil || closeErr != nil || string(encoded) != "preserve" {
			t.Fatalf("preserved %s entry = %q, read:%v close:%v", shardName, encoded, readErr, closeErr)
		}
	}
}

func TestCertifiedInitializationRacesConvergeOnOneExactScaffold(t *testing.T) {
	root := newMemoryDirectory()
	config, _ := certifiedFixture(t, root, checkpointmodel.AuthorityCreatedRoot, 0x31)

	start := make(chan struct{})
	errorsByWorker := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			namespace, err := Initialize(config)
			if err == nil {
				err = namespace.Close()
			}
			errorsByWorker <- err
		})
	}
	close(start)
	workers.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err == nil {
			continue
		}
		var repositoryErr *Error
		if !errors.As(err, &repositoryErr) || repositoryErr.Code() != ErrorBusy {
			t.Fatalf("initialization race error = %v", err)
		}
	}

	namespace, err := Initialize(config)
	if err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
	defer namespace.Close()
	control, err := root.OpenDirectory(ControlDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	checkpointRoot, err := control.OpenDirectory(CheckpointDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpointRoot.Close()
	names, err := checkpointRoot.Names(len(checkpointRootEntries) + 1)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	want := []string{LeasesDirectory, LookupDirectory, OperationsDirectory, OwnershipDirectory}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("checkpoint scaffold = %v, want %v", names, want)
	}
	ownershipDirectory, err := checkpointRoot.OpenDirectory(OwnershipDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ownershipDirectory.Close()
	encoded, err := ReadFile(ownershipDirectory, OwnershipFile)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := checkpointmodel.DecodeOwnership(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.Certification() != checkpointmodel.CertificationWindowsNTFSProcessRestart ||
		ownership.RootOpenDisposition() != checkpointmodel.AuthorityCreatedRoot {
		t.Fatalf("persisted certification = %q, disposition = %q",
			ownership.Certification(), ownership.RootOpenDisposition())
	}
	for name, spec := range map[string]checkpointmodel.OwnershipSpec{
		"certification": {
			Materializer:        ownership.MaterializerKind(),
			Certification:       checkpointmodel.CertificationLinuxExt4ProcessRestart,
			AuthorityRef:        ownership.AuthorityRef().Bytes(),
			RootOpenDisposition: ownership.RootOpenDisposition(),
		},
		"root disposition": {
			Materializer: ownership.MaterializerKind(), Certification: ownership.Certification(),
			AuthorityRef:        ownership.AuthorityRef().Bytes(),
			RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
		},
	} {
		t.Run("reject mismatched "+name, func(t *testing.T) {
			mismatched, err := checkpointmodel.NewOwnership(spec)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := OpenNamespace(CertifiedConfig{Root: root, Ownership: mismatched})
			if err == nil {
				_ = opened.Close()
			}
			if errorCode(err) != ErrorOwnershipMismatch {
				t.Fatalf("mismatched %s error = %v", name, err)
			}
		})
	}
}

func TestOperationContentionReturnsBusyBeforeOperationSubtreeAccess(t *testing.T) {
	root := newMemoryDirectory()
	config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x41)
	namespace, err := Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()

	baseOperations := namespace.operations
	operationAccesses := 0
	spy := &faultDirectory{Directory: baseOperations}
	spy.duplicate = func() (outputcap.Directory, error) { return spy, nil }
	spy.openDirectory = func(name string, private bool) (outputcap.Directory, error) {
		operationAccesses++
		return baseOperations.OpenDirectory(name, private)
	}
	spy.createDirectory = func(name string, private bool) (outputcap.Directory, error) {
		operationAccesses++
		return baseOperations.CreateDirectory(name, private)
	}
	namespace.operations = spy

	first, err := namespace.AcquireOperation(
		intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := first.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	defer first.Close()
	operationAccesses = 0

	if _, err := namespace.AcquireOperation(
		intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
	); errorCode(err) != ErrorBusy {
		t.Fatalf("contended lease error = %v", err)
	}
	if operationAccesses != 0 {
		t.Fatalf("contended acquisition touched operation subtree %d times", operationAccesses)
	}
	if kind, err := namespace.leases.ObserveEntry(
		operationLeaseName(intent.intent.OperationID()),
	); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("lease carrier = %v, %v", kind, err)
	}
}

func TestRepositoryCreateReplaceReopenAreOneDeterministicEngine(t *testing.T) {
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x61)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	forgedCandidate := checkpointRecordFixture(t, ownership, intent, 0x70)
	forgedVerified, err := checkpointmodel.PromoteInitialCandidate(forgedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(forgedVerified); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("predecessor-free verified create error = %v", err)
	}

	initial := checkpointRecordFixture(t, ownership, intent, 0x71)
	if err := repository.Create(initial); err != nil {
		t.Fatal(err)
	}
	reopened, err := repository.Reopen(initial.RecordID())
	if err != nil || !bytes.Equal(reopened.CanonicalBytes(), initial.CanonicalBytes()) {
		t.Fatalf("reopened initial record = %v, %v", reopened.RecordID(), err)
	}
	verified, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initial, verified); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(initial, verified); err != nil {
		t.Fatalf("idempotent replacement retry: %v", err)
	}
	if err := repository.Create(verified); err != nil {
		t.Fatalf("idempotent create retry: %v", err)
	}
	if err := repository.Create(initial); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("conflicting create error = %v", err)
	}
	reopened, err = repository.Reopen(initial.RecordID())
	if err != nil || reopened.CommitState() != checkpointmodel.CommitVerified {
		t.Fatalf("reopened replacement commit = %v, %v", reopened.CommitState(), err)
	}
}

func TestRepositoryReconcilesCandidateCrashCutsAndPreservesUnknownState(t *testing.T) {
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x81)
	defer namespace.Close()
	defer lease.Close()
	defer repository.Close()

	initial := checkpointRecordFixture(t, ownership, intent, 0x82)
	initialEncoded, err := checkpointmodel.EncodeRecord(initial)
	if err != nil {
		t.Fatal(err)
	}
	shardName, recordName := recordLocation(initial.RecordID())
	shard, err := OpenShard(repository.records, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	candidateName := TemporaryName(recordName, initialEncoded, 0)
	writeMemoryFile(t, shard, candidateName, initialEncoded)
	duplicateCandidateName := TemporaryName(recordName, initialEncoded, 1)
	writeMemoryFile(t, shard, duplicateCandidateName, initialEncoded)
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	witnessCalls := 0
	snapshot, err := repository.Reconcile(func(checkpointmodel.Record) (bool, error) {
		witnessCalls++
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if witnessCalls != 1 || len(snapshot.Records()) != 1 || len(snapshot.Attention()) != 0 ||
		snapshot.Records()[0].CommitState() != checkpointmodel.CommitVerified {
		t.Fatalf("initial reconciliation = calls:%d records:%d attention:%v", witnessCalls, len(snapshot.Records()), snapshot.Attention())
	}
	shard, err = OpenShard(repository.records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(shard, candidateName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("initial candidate survived: %v", err)
	}
	if _, err := ReadFile(shard, duplicateCandidateName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("duplicate initial candidate survived: %v", err)
	}
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}

	verified := snapshot.Records()[0]
	nextRecordCandidate, err := checkpointmodel.AdvanceGeneration(
		verified,
		[]checkpointmodel.Range{{Offset: 0, End: 16}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	nextEncoded, err := checkpointmodel.EncodeRecord(nextRecordCandidate)
	if err != nil {
		t.Fatal(err)
	}
	shard, err = OpenShard(repository.records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	nextCandidate := TemporaryName(recordName, nextEncoded, 1)
	writeMemoryFile(t, shard, nextCandidate, nextEncoded)
	writeMemoryFile(t, shard, "opaque-record", []byte("unknown"))
	writeMemoryFile(t, shard, ".candidate-foreign", []byte("unknown"))
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	unknownShard, err := repository.records.CreateDirectory("zz", true)
	if err != nil {
		t.Fatal(err)
	}
	writeMemoryFile(t, unknownShard, "owned-by-nobody", []byte("preserve"))
	if err := unknownShard.Close(); err != nil {
		t.Fatal(err)
	}
	wrongKindShard := "00"
	if wrongKindShard == shardName {
		wrongKindShard = "01"
	}
	writeMemoryFile(t, repository.records, wrongKindShard, []byte("not-a-shard"))

	snapshot, err = repository.Reconcile(func(checkpointmodel.Record) (bool, error) {
		t.Fatal("unlinked candidate unexpectedly requested a durability witness")
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records()) != 1 || snapshot.Records()[0].CheckpointGeneration() != verified.CheckpointGeneration() {
		t.Fatalf("unlinked candidate displaced verified state: %+v", snapshot.Records())
	}
	codes := make(map[AttentionCode]bool)
	for _, attention := range snapshot.Attention() {
		codes[attention.Code()] = true
	}
	for _, code := range []AttentionCode{AttentionUnknownShard, AttentionUnknownEntry, AttentionInvalidCandidate} {
		if !codes[code] {
			t.Fatalf("missing attention %q in %+v", code, snapshot.Attention())
		}
	}
	shard, err = OpenShard(repository.records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	for _, name := range []string{"opaque-record", ".candidate-foreign"} {
		if _, err := ReadFile(shard, name); err != nil {
			t.Fatalf("unknown entry %q was mutated: %v", name, err)
		}
	}
	if _, err := ReadFile(repository.records, wrongKindShard); err != nil {
		t.Fatalf("wrong-kind shard was mutated: %v", err)
	}
	if _, err := ReadFile(shard, nextCandidate); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unlinked candidate survived: %v", err)
	}
}

func certifiedFixture(
	t *testing.T,
	root outputcap.Directory,
	disposition checkpointmodel.RootOpenDisposition,
	fill byte,
) (CertifiedConfig, operationFixture) {
	t.Helper()
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{fill}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        authority.Bytes(),
		RootOpenDisposition: disposition,
	})
	if err != nil {
		t.Fatal(err)
	}
	var share catalog.ShareInstance
	var rootID catalog.DirectoryID
	share[0], rootID[0] = fill, fill+1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, rootID, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationID, err := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{fill + 2}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{fill + 3}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operationID, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkpointmodel.NewCLICompatibleOperationKey(selection, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := checkpointmodel.CLIReopenKey(key)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := checkpointmodel.NewReceiveOperation(intent, reopen)
	if err != nil {
		t.Fatal(err)
	}
	return CertifiedConfig{Root: root, Ownership: ownership}, operationFixture{
		intent: intent, operation: operation, binding: reservation.CanonicalBytes(),
	}
}

type operationFixture struct {
	intent    transfer.ReceiveIntent
	operation checkpointmodel.ReceiveOperation
	binding   []byte
}

func openRepositoryFixture(
	t *testing.T,
	fill byte,
) (*memoryDirectory, Namespace, OperationLease, Repository, checkpointmodel.Ownership, operationFixture) {
	t.Helper()
	root := newMemoryDirectory()
	config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, fill)
	namespace, err := Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireOperation(
		intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
	)
	if err != nil {
		namespace.Close()
		t.Fatal(err)
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		lease.Close()
		namespace.Close()
		t.Fatal(err)
	}
	if err := repository.InstallOperation(intent.operation, intent.binding); err != nil {
		repository.Close()
		lease.Close()
		namespace.Close()
		t.Fatal(err)
	}
	frozen, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: intent.intent.OperationID(), ReceiveIntent: intent.intent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		repository.Close()
		lease.Close()
		namespace.Close()
		t.Fatal(err)
	}
	if err := repository.CreateLifecycleState(frozen); err != nil {
		repository.Close()
		lease.Close()
		namespace.Close()
		t.Fatal(err)
	}
	if err := lease.RegisterLookup(intent.operation); err != nil {
		repository.Close()
		lease.Close()
		namespace.Close()
		t.Fatal(err)
	}
	return root, namespace, lease, repository, config.Ownership, intent
}

func checkpointRecordFixture(
	t *testing.T,
	ownership checkpointmodel.Ownership,
	intent operationFixture,
	fill byte,
) checkpointmodel.Record {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = fill
		revision[index] = fill + 1
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  intent.intent.OperationID(),
		ReceiveIntentDigest:          intent.intent.Digest(),
		MaterializationBindingDigest: intent.intent.BindingDigest(),
		FileID:                       fileID,
		FileRevision:                 revision,
		CanonicalPath:                "folder/file.bin",
		ExactSize:                    64,
		MaterializerKind:             ownership.MaterializerKind(),
		AuthorityRef:                 ownership.AuthorityRef().Bytes(),
		OwnedObjectID:                bytes.Repeat([]byte{fill + 2}, sha256.Size),
		StateGeneration:              1,
		CheckpointGeneration:         0,
		Phase:                        checkpointmodel.PhaseActive,
		CommitState:                  checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func errorCode(err error) ErrorCode {
	if repositoryErr, ok := errors.AsType[*Error](err); ok {
		return repositoryErr.Code()
	}
	return ""
}
