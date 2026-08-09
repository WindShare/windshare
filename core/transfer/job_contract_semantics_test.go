package transfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestTransferJobFailureTaxonomyPreservesScopeAndCause(t *testing.T) {
	permanent := sourcePermanentFailure(errors.New("file source failed permanently"))
	if got := normalizedFault(permanent); got.Domain() != fault.DomainSource ||
		got.Scope() != fault.ScopeFileLocal {
		t.Fatalf("permanent source failure = %v", got)
	}
	if fileRetireReason(permanent) != FileRetireIsolatedPermanentSourceFailure {
		t.Fatal("isolated permanent source failure did not authorize retirement")
	}

	session := sessionProtocolFailure(errors.New("protocol session failed"))
	if got := normalizedFault(session); got.Domain() != fault.DomainSession ||
		got.Scope() != fault.ScopeSessionTerminal || !isJobTerminalError(session) {
		t.Fatalf("session failure = %v", got)
	}

	budget := resourceBudgetFailure(errors.New("resource budget exceeded"))
	if got := normalizedFault(budget); got.Domain() != fault.DomainSession ||
		got.Scope() != fault.ScopeOutputPause || !isJobTerminalError(budget) {
		t.Fatalf("resource budget failure = %v", got)
	}

	dependency := dependencyContractFailure(errors.New("dependency contract violated"))
	if got := normalizedFault(dependency); got != fault.DependencyContractFault() || !isJobTerminalError(dependency) {
		t.Fatalf("dependency contract failure = %v", got)
	}
	if filePauseReason(budget) != FilePauseResourceBudget ||
		jobPauseReason(budget, nil) != JobPauseResourceBudget {
		t.Fatal("resource budget failure was persisted as a foreign failure class")
	}
	if filePauseReason(dependency) != FilePauseDependencyContract ||
		jobPauseReason(dependency, nil) != JobPauseDependencyContract {
		t.Fatal("dependency contract failure was persisted as a foreign failure class")
	}
}

func TestTransferJobDirectTreeSessionAndFileValidatorsFailClosed(t *testing.T) {
	share := transferID[catalog.ShareInstance](161)
	root := transferID[catalog.DirectoryID](162)
	rules, _ := NewSelectionRules(true, nil)
	intent := testReceiveIntent(t, share, root, rules)
	if err := validateDirectTreeSession(intent, nil); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
		t.Fatalf("nil output session error = %v", err)
	}
	output := newJobOutput(share)
	opened, err := output.OpenDirectTree(context.Background(), intent)
	if err != nil || opened != output {
		t.Fatalf("open direct tree session = %T, %v", opened, err)
	}
	if err := validateDirectTreeSession(intent, output); err != nil {
		t.Fatalf("valid output session error = %v", err)
	}
	sessionBinding := output.Binding()
	if sessionBinding.ReceiveIntentDigest() != intent.Digest() ||
		sessionBinding.OperationID() != intent.OperationID() ||
		sessionBinding.BindingDigest() != intent.BindingDigest() {
		t.Fatalf("direct tree session binding = %+v", sessionBinding)
	}
	if _, err := BindDirectTreeSession(ReceiveIntent{}); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("zero intent binding error = %v", err)
	}
	validBinding := output.binding
	output.binding.operationID[0] ^= 0xff
	if err := validateDirectTreeSession(intent, output); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
		t.Fatalf("foreign intent binding error = %v", err)
	}
	output.binding = validBinding
	output.session = OutputSessionID{}
	if err := validateDirectTreeSession(intent, output); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
		t.Fatalf("zero output session identity error = %v", err)
	}

	binding, checkpoint := outputLifecycleFixture(t)
	target := binding.Target()
	transaction := &jobFileTransaction{binding: binding}
	if err := validateOutputTransaction(target, nil, checkpoint); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
		t.Fatalf("nil output transaction error = %v", err)
	}
	if err := validateOutputTransaction(target, transaction, checkpoint); err != nil {
		t.Fatalf("valid output transaction error = %v", err)
	}
	otherIdentity := binding.ObjectIdentity()
	otherIdentity[0] ^= 0xff
	otherBinding, err := BindFileMaterializationTarget(target, otherIdentity)
	if err != nil {
		t.Fatal(err)
	}
	otherCheckpoint, err := VerifyDurableRanges(otherBinding, checkpoint.CheckpointGeneration(), checkpoint.Ranges())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOutputTransaction(target, transaction, otherCheckpoint); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
		t.Fatalf("transaction with foreign checkpoint error = %v", err)
	}
	if err := validateMaterializedFileBinding(FileMaterializationTarget{}, binding); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("foreign output binding error = %v", err)
	}
	if err := validateMaterializedFileBinding(target, binding); err != nil {
		t.Fatalf("valid output binding error = %v", err)
	}

	for name, settlement := range immediateDirectTreeSettlements(t, binding, checkpoint) {
		t.Run(name, func(t *testing.T) {
			if err := validateImmediateFileSettlement(target, settlement); err != nil {
				t.Fatalf("valid immediate settlement error = %v", err)
			}
		})
	}
	if err := validateImmediateFileSettlement(target, FileSettlement{}); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("zero immediate settlement error = %v", err)
	}
	paused, err := NewVerifiedFileSettlement(FilePaused, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateImmediateFileSettlement(target, paused); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("paused immediate settlement error = %v", err)
	}
}

func TestTransferJobDirectorySettlementValidatorRequiresExactAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xa1)
	root := transferID[catalog.DirectoryID](0xa2)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewDirectoryAdmissionScope(testReceiveIntent(t, share, root, rules))
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, directoryAdmissionSecretBytes)
	secret[0] = 1
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, MaterializationDirectory{
		DirectoryID: root,
		Generation:  transferID[catalog.DirectoryGeneration](0xa3),
	})
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, MaterializationDirectory{
		DirectoryID:     transferID[catalog.DirectoryID](0xa4),
		Generation:      transferID[catalog.DirectoryGeneration](0xa5),
		ParentAdmission: rootAdmission,
		Path:            "child",
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := NewFinalizedDirectorySettlement(childAdmission)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDirectorySettlement(childAdmission, finalized); err != nil {
		t.Fatalf("exact finalized settlement = %v", err)
	}
	metadataFault, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := NewIsolatedDirectorySettlement(childAdmission, metadataFault)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDirectorySettlement(childAdmission, isolated); err != nil {
		t.Fatalf("exact isolated settlement = %v", err)
	}

	foreign, err := NewFinalizedDirectorySettlement(rootAdmission)
	if err != nil {
		t.Fatal(err)
	}
	tamperedAdmission := childAdmission
	tamperedAdmission.path = "other"
	tampered, err := NewFinalizedDirectorySettlement(tamperedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	for name, settlement := range map[string]DirectorySettlement{
		"zero":                {},
		"foreign receipt":     foreign,
		"tampered projection": tampered,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDirectorySettlement(childAdmission, settlement); normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
				t.Fatalf("settlement error = %v", err)
			}
		})
	}
}

func TestTransferJobImmediateSettlementsPreserveOutcomeAndReleaseFailures(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)
	opened := OpenedRevision{
		LeaseID:    transferID[content.LeaseID](162),
		Descriptor: binding.Descriptor(),
	}
	plan := plannedFile{file: binding.FileID(), path: binding.Locator().CanonicalPath()}

	for name, settlement := range immediateDirectTreeSettlements(t, binding, checkpoint) {
		t.Run(name, func(t *testing.T) {
			revisions := &jobRevisionClient{}
			run := immediateSettlementJobRun(binding.ShareInstance(), revisions)
			err := run.handleImmediateSettlement(context.Background(), plan, opened, settlement)
			if err != nil {
				t.Fatalf("immediate settlement error = %v", err)
			}
			if settlement.Kind() == FilePublished {
				if run.succeeded != 1 || len(run.files) != 0 {
					t.Fatalf("published result succeeded=%d failures=%+v", run.succeeded, run.files)
				}
				return
			}
			if len(run.files) != 1 || run.files[0].Settlement.Kind() != settlement.Kind() {
				t.Fatalf("immediate failure record = %+v", run.files)
			}
			wantCause := map[FileSettlementKind]error{
				FileCollision:      ErrOutputPublishBlocked,
				FilePublishBlocked: ErrOutputPublishBlocked,
				FileQuarantined:    ErrOutputQuarantined,
				FileRetired:        ErrOutputRetired,
			}[settlement.Kind()]
			if !errors.Is(run.files[0].Cause, wantCause) {
				t.Fatalf("immediate failure cause = %v, want %v", run.files[0].Cause, wantCause)
			}
		})
	}

	t.Run("terminal lease release", func(t *testing.T) {
		cause := errors.New("session ended during release")
		revisions := &jobRevisionClient{releaseErr: sessionProtocolFailure(cause)}
		run := immediateSettlementJobRun(binding.ShareInstance(), revisions)
		published := immediateDirectTreeSettlements(t, binding, checkpoint)["published"]
		err := run.handleImmediateSettlement(context.Background(), plan, opened, published)
		if normalizedFault(err) != mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol) ||
			run.succeeded != 1 || len(run.files) != 1 ||
			run.files[0].Stage != FailureLeaseRelease {
			t.Fatalf("terminal release result error=%v succeeded=%d failures=%+v", err, run.succeeded, run.files)
		}
	})

	t.Run("terminal lease release after collision", func(t *testing.T) {
		cause := errors.New("session ended during collision release")
		revisions := &jobRevisionClient{releaseErr: sessionProtocolFailure(cause)}
		run := immediateSettlementJobRun(binding.ShareInstance(), revisions)
		collision := immediateDirectTreeSettlements(t, binding, checkpoint)["collision"]
		err := run.handleImmediateSettlement(context.Background(), plan, opened, collision)
		if normalizedFault(err) != mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol) || len(run.files) != 1 ||
			!errors.Is(run.files[0].Cause, ErrOutputPublishBlocked) {
			t.Fatalf("terminal collision release error=%v failures=%+v", err, run.files)
		}
	})

	t.Run("unknown immediate kind", func(t *testing.T) {
		run := immediateSettlementJobRun(binding.ShareInstance(), &jobRevisionClient{})
		err := run.handleImmediateSettlement(
			context.Background(),
			plan,
			opened,
			FileSettlement{kind: FilePaused, target: binding.Target()},
		)
		if normalizedFault(err) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
			normalizedFault(run.settlementFailure) != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) {
			t.Fatalf("unknown immediate settlement error = %v, settlement failure = %v", err, run.settlementFailure)
		}
	})
}

func TestTransferJobRejectsUnstartedFileWithoutLeakingRevisionLease(t *testing.T) {
	cause := errors.New("output locator could not be represented")
	releaseCause := errors.New("lease release failed")
	revisions := &jobRevisionClient{releaseErr: releaseCause}
	run := immediateSettlementJobRun(transferID[catalog.ShareInstance](163), revisions)
	lease := transferID[content.LeaseID](164)
	plan := plannedFile{file: transferID[catalog.FileID](165), path: "file.bin"}
	err := run.rejectUnstartedFile(
		context.Background(), plan, OpenedRevision{LeaseID: lease}, dependencyContractFailure(cause),
	)
	if normalizedFault(err) != fault.DependencyContractFault() || len(revisions.released) != 1 ||
		revisions.released[0] != lease || len(run.files) != 1 ||
		run.files[0].LeaseReleaseFault != fault.DependencyContractFault() {
		t.Fatalf("unstarted file rejection error=%v released=%v failures=%+v", err, revisions.released, run.files)
	}
}

func TestAtomicRequestedRangeSinkCancellationAndTerminalStateFailClosed(t *testing.T) {
	var nilSink *atomicRequestedRangeSink
	if err := nilSink.Flush(context.Background()); normalizedFault(err) != fault.DependencyContractFault() || !isJobTerminalError(err) {
		t.Fatalf("nil atomic sink flush error = %v", err)
	}
	if err := nilSink.Failure(); normalizedFault(err) != fault.DependencyContractFault() || !isJobTerminalError(err) {
		t.Fatalf("nil atomic sink failure = %v", err)
	}

	targetWrites := 0
	target := RangeSinkFunc(func(context.Context, uint64, []byte) error {
		targetWrites++
		return nil
	})
	cancelled, err := newAtomicRequestedRangeSink(content.Range{Offset: 10, End: 12}, target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelled.WriteRange(ctx, 10, []byte{1, 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled atomic write error = %v", err)
	}
	if err := cancelled.WriteRange(context.Background(), 10, []byte{1, 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("atomic sink did not retain cancellation = %v", err)
	}
	if !errors.Is(cancelled.Failure(), context.Canceled) || targetWrites != 0 {
		t.Fatalf("cancelled sink failure = %v, target writes = %d", cancelled.Failure(), targetWrites)
	}
	if err := cancelled.Flush(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sink flush did not retain failure = %v", err)
	}

	sealed, err := newAtomicRequestedRangeSink(content.Range{Offset: 20, End: 22}, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.WriteRange(context.Background(), 20, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := sealed.Flush(context.Background()); err != nil || targetWrites != 1 {
		t.Fatalf("first atomic flush error = %v, target writes = %d", err, targetWrites)
	}
	postFlush, err := newAtomicRequestedRangeSink(content.Range{Offset: 30, End: 32}, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := postFlush.WriteRange(context.Background(), 30, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := postFlush.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := postFlush.WriteRange(context.Background(), 30, []byte{1, 2}); normalizedFault(err) != fault.DependencyContractFault() || !isJobTerminalError(err) {
		t.Fatalf("post-flush atomic write error = %v", err)
	}
	if err := sealed.Flush(context.Background()); normalizedFault(err) != fault.DependencyContractFault() || !isJobTerminalError(err) {
		t.Fatalf("duplicate atomic flush error = %v", err)
	}
	if err := sealed.WriteRange(context.Background(), 20, []byte{1, 2}); normalizedFault(err) != fault.DependencyContractFault() {
		t.Fatalf("post-flush atomic write error = %v", err)
	}
}

func TestTransferJobRejectsInvalidAdmissionAndRevisionIdentityTransitions(t *testing.T) {
	t.Run("invalid admitted output session", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](166)
		output := newJobOutput(share)
		output.session = OutputSessionID{}
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomeResumable ||
			result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
			output.pauseCalls != 0 || output.completeCalls != 0 {
			t.Fatalf("invalid admission result=%+v pause=%d complete=%d", result, output.pauseCalls, output.completeCalls)
		}
	})

	t.Run("modified time is part of revision identity", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](167)
		file := transferID[catalog.FileID](168)
		entryTime, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
		if err != nil {
			t.Fatal(err)
		}
		descriptorTime, err := catalog.NewModifiedTime(1_700_000_001, 0, catalog.TimePrecisionSeconds)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := catalog.NewFileEntry(file, "file.bin", 1, entryTime)
		if err != nil {
			t.Fatal(err)
		}
		geometry, err := content.NewFileGeometry(1, catalog.MinChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(
			share, file, transferID[content.FileRevision](169), geometry, descriptorTime,
		)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := NewOpenedRevision(transferID[content.LeaseID](170), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateOpenedFile(share, entry, opened); !errors.Is(err, ErrRevisionIdentity) {
			t.Fatalf("modified-time identity error = %v", err)
		}
	})

	t.Run("terminal release while rejecting revision identity", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](171)
		selected := transferID[catalog.FileID](172)
		foreign := transferID[catalog.FileID](173)
		lease := transferID[content.LeaseID](174)
		descriptor := jobDescriptor(t, share, foreign, 175, 1)
		opened, err := NewOpenedRevision(lease, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		cause := errors.New("session ended while releasing rejected revision")
		revisions := &jobRevisionClient{
			opened:   map[catalog.FileID]OpenedRevision{selected: opened},
			failures: make(map[catalog.FileID]error), releaseErr: sessionProtocolFailure(cause),
		}
		run := immediateSettlementJobRun(share, revisions)
		entry := jobEntry(t, selected, "file.bin", 1)
		plan := plannedFile{
			file: selected, path: "file.bin", expectedSize: entry.ExpectedSize(), modified: entry.ModifiedTime(),
		}
		_, ready, err := run.openSelectedRevision(context.Background(), plan)
		if ready || normalizedFault(err) != mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol) ||
			len(revisions.released) != 1 || revisions.released[0] != lease {
			t.Fatalf("rejected revision ready=%v error=%v released=%v", ready, err, revisions.released)
		}
	})
}

func TestRangesContainAdvancesAcrossDisjointDurableSegments(t *testing.T) {
	available, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 2}, {Offset: 3, End: 7}})
	if err != nil {
		t.Fatal(err)
	}
	required, err := content.NewRangeSet([]content.Range{{Offset: 3, End: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if !rangesContain(available, required) {
		t.Fatal("later durable segment did not satisfy the required range")
	}
}

func immediateSettlementJobRun(share catalog.ShareInstance, revisions RevisionClient) *jobRun {
	return &jobRun{
		job: &TransferJob{
			revisions: revisions, settlementTimeout: time.Second,
		},
		output: newJobOutput(share),
	}
}

func immediateDirectTreeSettlements(
	t *testing.T,
	binding MaterializedFileBinding,
	checkpoint VerifiedDurableRanges,
) map[string]FileSettlement {
	t.Helper()
	published, err := NewVerifiedFileSettlement(FilePublished, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	publishBlocked, err := NewVerifiedFileSettlement(FilePublishBlocked, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	collision, err := NewCollisionFileSettlement(binding.Target())
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}
	quarantined, err := NewImmediateQuarantinedFileSettlement(
		binding.Target(),
		reference,
		QuarantineOwnershipMismatch,
	)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := NewRetiredFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]FileSettlement{
		"published": published, "collision": collision, "publish blocked": publishBlocked,
		"quarantined": quarantined, "retired": retired,
	}
}
