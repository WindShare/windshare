//go:build windows || linux

package osfs

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const retainedCandidateExactSize = 8

type retainedCandidateFixture struct {
	root      string
	intent    transfer.ReceiveIntent
	candidate checkpointmodel.Record
	rootSeed  byte
	fileSeed  byte
}

func runRetainedCandidateFacadeProof(t *testing.T) {
	t.Helper()

	listFixture := newRetainedCandidateFixture(t, "list-from-candidate", 0x71)
	listResume := listRetainedCandidate(t, listFixture)
	assertRetainedCandidatePromoted(t, listFixture)
	discardRetainedCandidate(t, listResume, listFixture.intent.OperationID())

	getFixture := newRetainedCandidateFixture(t, "get-from-candidate", 0x81)
	reopenRetainedCandidateOutput(t, getFixture, false)
	assertRetainedCandidatePromoted(t, getFixture)
	reopenRetainedCandidateOutput(t, getFixture, true)

	sharedFixture := newRetainedCandidateFixture(t, "list-then-get", 0x91)
	sharedResume := listRetainedCandidate(t, sharedFixture)
	assertRetainedCandidatePromoted(t, sharedFixture)
	reopenRetainedCandidateOutput(t, sharedFixture, true)
	discardRetainedCandidate(t, sharedResume, sharedFixture.intent.OperationID())
}

func newRetainedCandidateFixture(
	t *testing.T,
	name string,
	seed byte,
) retainedCandidateFixture {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), name)
	output, operation, intent := openPublicOrdinaryOperation(t, root, seed)
	session, err := output.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	rootSeed := seed + 1
	fileSeed := seed + 2
	rootAdmission := admitRetainedCandidateRoot(t, session, intent, rootSeed)
	file := nativeDirectTreeTestFile(
		t, session, intent, fileSeed, "candidate.bin", retainedCandidateExactSize, rootAdmission,
	)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("retained candidate file settled before receiving content")
	}
	if err := transaction.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	durable, err := transaction.Checkpoint(ctx)
	if err != nil || len(durable.Ranges().Ranges()) != 1 {
		t.Fatalf("initial checkpoint = (%v, %v)", durable.Ranges().Ranges(), err)
	}
	if settlement, err := transaction.Pause(ctx, transfer.FilePauseInterrupted); err != nil ||
		settlement.Kind() != transfer.FilePaused {
		t.Fatalf("initial file pause = (%d, %v)", settlement.Kind(), err)
	}
	if settlement, err := session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("initial tree pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	candidate := retainSemanticCandidate(t, root, intent)
	return retainedCandidateFixture{
		root: root, intent: intent, candidate: candidate,
		rootSeed: rootSeed, fileSeed: fileSeed,
	}
}

func listRetainedCandidate(
	t *testing.T,
	fixture retainedCandidateFixture,
) ResumeStateAuthority {
	t.Helper()
	resume, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: fixture.root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resume.ListResumeState(context.Background())
	summaries := inventory.Summaries()
	if err != nil || inventory.Status() != ResumeStateListReady ||
		len(summaries) != 1 || summaries[0].OperationID() != fixture.intent.OperationID() ||
		summaries[0].State() != ResumeOperationResumable || !summaries[0].Resumable() ||
		summaries[0].ReceiveIntentDigest() != fixture.intent.Digest() {
		t.Fatalf("retained candidate inventory = (%+v, %v)", inventory, err)
	}
	return resume
}

func reopenRetainedCandidateOutput(
	t *testing.T,
	fixture retainedCandidateFixture,
	continueFile bool,
) {
	t.Helper()
	ctx := context.Background()
	reopened, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: fixture.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	mode, err := reopened.BindDestination(ctx)
	if err != nil || !mode.Resumable() {
		t.Fatalf("reopen bind = (%+v, %v)", mode, err)
	}
	lookup, err := reopened.LookupActive(ctx, fixture.intent.SelectionSpec())
	if err != nil || lookup.Kind() != FilesystemOutputLookupReopened ||
		lookup.StateReason() != 0 {
		t.Fatalf("retained candidate lookup = (%d, %s, %v)",
			lookup.Kind(), lookup.StateReason(), err)
	}
	reopenedIntent, present := lookup.Operation().ReceiveIntent()
	if !present || !reopenedIntent.EqualCanonical(fixture.intent) ||
		!lookup.Operation().ExecutionMode().Resumable() {
		t.Fatalf("retained candidate operation identity = (%+v, %t)", reopenedIntent, present)
	}
	resumedSession, err := reopened.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	if !continueFile {
		if settlement, pauseErr := resumedSession.PauseTree(ctx, transfer.JobPauseInterrupted); pauseErr != nil ||
			settlement.Kind() != transfer.DirectTreeSettlementPaused {
			t.Fatalf("candidate proof tree pause = (%d, %v)", settlement.Kind(), pauseErr)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	resumedRoot := admitRetainedCandidateRoot(t, resumedSession, fixture.intent, fixture.rootSeed)
	resumedFile := nativeDirectTreeTestFile(
		t, resumedSession, fixture.intent, fixture.fileSeed,
		"candidate.bin", retainedCandidateExactSize, resumedRoot,
	)
	resumedStart, err := resumedSession.BeginFile(ctx, resumedFile)
	if err != nil {
		t.Fatal(err)
	}
	resumedTransaction, resumedDurable, ok := resumedStart.Transaction()
	ranges := resumedDurable.Ranges().Ranges()
	if !ok || len(ranges) != 1 || ranges[0].Offset != 0 ||
		ranges[0].End != uint64(len("partdo")) {
		t.Fatalf("resumed durable ranges = (%v, transaction=%t)", ranges, ok)
	}
	if err := resumedTransaction.WriteRange(ctx, uint64(len("partdo")), []byte("ne")); err != nil {
		t.Fatal(err)
	}
	if _, err := resumedTransaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if settlement, err := resumedTransaction.Pause(ctx, transfer.FilePauseInterrupted); err != nil ||
		settlement.Kind() != transfer.FilePaused {
		t.Fatalf("resumed file pause = (%d, %v)", settlement.Kind(), err)
	}
	if settlement, err := resumedSession.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("resumed tree pause = (%d, %v)", settlement.Kind(), err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func discardRetainedCandidate(
	t *testing.T,
	resume ResumeStateAuthority,
	operation receivecontract.OperationID,
) {
	t.Helper()
	exact, err := resume.ListResumeState(context.Background())
	exactSummaries := exact.Summaries()
	if err != nil || len(exactSummaries) != 1 ||
		exactSummaries[0].OperationID() != operation || !exactSummaries[0].Resumable() {
		t.Fatalf("exact pre-discard snapshot = (%+v, %v)", exact, err)
	}
	discarded, err := resume.Discard(context.Background(), operation)
	if err != nil || discarded.State() != ResumeOperationDiscarded ||
		discarded.OperationID() != operation {
		t.Fatalf("exact discard = (%+v, %v)", discarded, err)
	}
}

func assertRetainedCandidatePromoted(t *testing.T, fixture retainedCandidateFixture) {
	t.Helper()
	repository, _, closeRepository := openRetainedCandidateRepository(t, fixture.root, fixture.intent)
	defer closeRepository()
	promoted, err := repository.Reopen(fixture.candidate.RecordID())
	if err != nil {
		t.Fatal(err)
	}
	if promoted.CommitState() != checkpointmodel.CommitVerified ||
		promoted.Phase() != fixture.candidate.Phase() ||
		promoted.StateGeneration() != fixture.candidate.StateGeneration() ||
		promoted.CheckpointGeneration() != fixture.candidate.CheckpointGeneration() ||
		promoted.OperationID() != fixture.intent.OperationID() ||
		promoted.ReceiveIntentDigest() != fixture.intent.Digest() ||
		!slices.Equal(promoted.VerifiedRanges(), fixture.candidate.VerifiedRanges()) {
		t.Fatalf("promoted candidate = %+v, want identity/generation/ranges from %+v", promoted, fixture.candidate)
	}
}

func admitRetainedCandidateRoot(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seed byte,
) transfer.DirectoryAdmission {
	t.Helper()
	request, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  coverageC6Identity[catalog.DirectoryGeneration](seed),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func retainSemanticCandidate(
	t *testing.T,
	root string,
	intent transfer.ReceiveIntent,
) checkpointmodel.Record {
	t.Helper()
	repository, profile, closeRepository := openRetainedCandidateRepository(t, root, intent)
	defer closeRepository()
	store, err := checkpointstore.NewFileExecutionStoreWithProfile(repository, profile)
	if err != nil {
		t.Fatal(err)
	}
	records, _ := store.Snapshot()
	if len(records) != 1 {
		t.Fatalf("retained candidate record count = %d, want 1", len(records))
	}
	stable := records[0]
	active, err := checkpointmodel.AdvanceState(
		stable,
		stable.StateGeneration()+1,
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitVerified,
		stable.QuarantineReason(),
		stable.QuarantineOrigin(),
		stable.RetirementReason(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(stable, active); err != nil {
		t.Fatal(err)
	}
	owned, _, err := store.OpenOwnedFile(
		context.Background(), active.OwnedObjectID(), active.ExactSize(), true,
	)
	if err != nil || owned == nil {
		t.Fatalf("open retained stage = (%T, %v)", owned, err)
	}
	if written, err := owned.WriteAt([]byte("do"), int64(len("part"))); err != nil ||
		written != len("do") {
		t.Fatalf("extend retained stage = (%d, %v)", written, err)
	}
	// Closing without Sync reproduces the semantic cut: the candidate record is
	// durable while recovery still owns the stage durability decision.
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	candidate, err := checkpointmodel.AdvanceGeneration(
		active,
		[]checkpointmodel.Range{{Offset: 0, End: uint64(len("partdo"))}},
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(active, candidate); err != nil {
		t.Fatal(err)
	}
	reopened, err := repository.Reopen(candidate.RecordID())
	if err != nil || reopened.CommitState() != checkpointmodel.CommitCandidate {
		t.Fatalf("semantic candidate cut = (%+v, %v)", reopened, err)
	}
	return reopened
}

func openRetainedCandidateRepository(
	t *testing.T,
	root string,
	intent transfer.ReceiveIntent,
) (*checkpointstore.Repository, checkpointmodel.LiveCleanupNativeProfile, func()) {
	t.Helper()
	platform, err := openNativeOutputPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := platform.Root().OpenDirectory(checkpointstore.ControlDirectory, true)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	registry, err := checkpointstore.OpenOperationRegistry(control)
	if err != nil {
		_ = errors.Join(control.Close(), platform.Close())
		t.Fatal(err)
	}
	lease, err := registry.AcquireOperationLease(intent.OperationID())
	if err != nil {
		_ = errors.Join(registry.Close(), control.Close(), platform.Close())
		t.Fatal(err)
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		t.Fatal("retained candidate intent omitted its destination reservation")
	}
	var disposition outputcap.RootOpenDisposition
	switch reservation.EntryKind() {
	case receivecontract.ContainerEntrySingleFile:
		disposition = outputcap.CallerProvidedContainer
	case receivecontract.ContainerEntryResultRoot:
		disposition = outputcap.AuthorityCreatedRoot
	default:
		t.Fatalf("unexpected retained-candidate entry kind %d", reservation.EntryKind())
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       platform.Certification(),
		AuthorityRef:        reservation.AuthorityRef().Bytes(),
		RootOpenDisposition: disposition,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(
		ownership,
		intent.OperationID(),
		intent.Digest(),
		intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repositoryValue, err := checkpointstore.OpenOrdinaryFileRepository(lease, binding, false)
	if err != nil {
		_ = errors.Join(lease.Close(), registry.Close(), control.Close(), platform.Close())
		t.Fatal(err)
	}
	repository := &repositoryValue
	var profile checkpointmodel.LiveCleanupNativeProfile
	switch platform.Certification() {
	case outputcap.CertificationWindowsNTFSProcessRestart:
		profile = checkpointmodel.LiveCleanupWindowsNTFSV1
	case outputcap.CertificationLinuxExt4ProcessRestart:
		profile = checkpointmodel.LiveCleanupLinuxExt4V1
	default:
		_ = errors.Join(repository.Close(), lease.Close(), registry.Close(), control.Close(), platform.Close())
		t.Fatalf("unexpected retained-candidate certification %q", platform.Certification())
	}
	closeRepository := func() {
		if closeErr := errors.Join(
			repository.Close(),
			lease.Close(),
			registry.Close(),
			control.Close(),
			platform.Close(),
		); closeErr != nil {
			t.Errorf("close retained-candidate repository: %v", closeErr)
		}
	}
	return repository, profile, closeRepository
}
