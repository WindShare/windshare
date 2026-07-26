package transfer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestTransferJobFailureTaxonomyPreservesScopeAndCause(t *testing.T) {
	permanent := NewIsolatedPermanentSourceFailure(nil)
	var permanentFailure *IsolatedPermanentSourceFailureError
	if !errors.As(permanent, &permanentFailure) || errors.Unwrap(permanent) == nil ||
		!strings.Contains(permanent.Error(), "file source failed permanently") {
		t.Fatalf("permanent source failure = %v", permanent)
	}
	permanentFailure.IsolatedPermanentSourceFailure()
	if fileRetireReason(permanent) != FileRetireIsolatedPermanentSourceFailure {
		t.Fatal("isolated permanent source failure did not authorize retirement")
	}

	session := NewSessionFailure(nil)
	var sessionFailure *SessionFailureError
	if !errors.As(session, &sessionFailure) || errors.Unwrap(session) == nil || !IsSessionFailure(session) ||
		!strings.Contains(session.Error(), "protocol session failed") {
		t.Fatalf("session failure = %v", session)
	}
	sessionFailure.SessionFailure()

	budget := NewJobResourceBudgetError(nil)
	var budgetFailure *JobResourceBudgetError
	if !errors.As(budget, &budgetFailure) || errors.Unwrap(budget) == nil || !isJobFatal(budget) ||
		!strings.Contains(budget.Error(), "resource budget exceeded") {
		t.Fatalf("resource budget failure = %v", budget)
	}
	budgetFailure.JobFatal()

	dependency := NewJobDependencyContractError(nil)
	var dependencyFailure *JobDependencyContractError
	if !errors.As(dependency, &dependencyFailure) || errors.Unwrap(dependency) == nil || !isJobFatal(dependency) ||
		!strings.Contains(dependency.Error(), "dependency contract violated") {
		t.Fatalf("dependency contract failure = %v", dependency)
	}
	dependencyFailure.JobFatal()
}

func TestTransferJobOutputSessionAndFileValidatorsFailClosed(t *testing.T) {
	if err := validateOutputSession(nil); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("nil output session error = %v", err)
	}
	output := newJobOutput(transferID[catalog.ShareInstance](161))
	if err := validateOutputSession(output); err != nil {
		t.Fatalf("valid output session error = %v", err)
	}
	output.session = OutputSessionID{}
	if err := validateOutputSession(output); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("zero output session identity error = %v", err)
	}

	binding, checkpoint := outputLifecycleFixture(t)
	target := binding.Target()
	transaction := &jobFileTransaction{binding: binding}
	if err := validateOutputTransaction(target, nil, checkpoint); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("nil output transaction error = %v", err)
	}
	if err := validateOutputTransaction(target, transaction, checkpoint); err != nil {
		t.Fatalf("valid output transaction error = %v", err)
	}
	otherIdentity := binding.ObjectIdentity()
	otherIdentity[0] ^= 0xff
	otherBinding, err := BindOutputFileTarget(target, otherIdentity)
	if err != nil {
		t.Fatal(err)
	}
	otherCheckpoint, err := VerifyDurableRanges(otherBinding, checkpoint.CheckpointGeneration(), checkpoint.Ranges())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOutputTransaction(target, transaction, otherCheckpoint); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("transaction with foreign checkpoint error = %v", err)
	}
	if err := validateOutputFileBinding(OutputFileTarget{}, binding); !errors.Is(err, ErrOutputContract) {
		t.Fatalf("foreign output binding error = %v", err)
	}
	if err := validateOutputFileBinding(target, binding); err != nil {
		t.Fatalf("valid output binding error = %v", err)
	}

	for name, settlement := range immediateJobSettlements(t, binding, checkpoint) {
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

func TestTransferJobImmediateSettlementsPreserveOutcomeAndReleaseFailures(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)
	opened := OpenedRevision{
		LeaseID:    transferID[content.LeaseID](162),
		Descriptor: binding.Descriptor(),
	}
	plan := plannedFile{file: binding.FileID(), path: binding.Locator().CanonicalPath()}

	for name, settlement := range immediateJobSettlements(t, binding, checkpoint) {
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
		revisions := &jobRevisionClient{releaseErr: NewSessionFailure(cause)}
		run := immediateSettlementJobRun(binding.ShareInstance(), revisions)
		published := immediateJobSettlements(t, binding, checkpoint)["published"]
		err := run.handleImmediateSettlement(context.Background(), plan, opened, published)
		if !errors.Is(err, cause) || run.succeeded != 1 || len(run.files) != 1 ||
			run.files[0].Stage != FailureLeaseRelease {
			t.Fatalf("terminal release result error=%v succeeded=%d failures=%+v", err, run.succeeded, run.files)
		}
	})

	t.Run("terminal lease release after collision", func(t *testing.T) {
		cause := errors.New("session ended during collision release")
		revisions := &jobRevisionClient{releaseErr: NewSessionFailure(cause)}
		run := immediateSettlementJobRun(binding.ShareInstance(), revisions)
		collision := immediateJobSettlements(t, binding, checkpoint)["collision"]
		err := run.handleImmediateSettlement(context.Background(), plan, opened, collision)
		if !errors.Is(err, cause) || len(run.files) != 1 ||
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
		if !errors.Is(err, ErrOutputContract) || run.settlementFailure == nil {
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
	err := run.rejectUnstartedFile(context.Background(), plan, OpenedRevision{LeaseID: lease}, cause)
	if !errors.Is(err, cause) || !errors.Is(err, releaseCause) || len(revisions.released) != 1 ||
		revisions.released[0] != lease || len(run.files) != 1 || run.files[0].LeaseReleaseFailure != releaseCause {
		t.Fatalf("unstarted file rejection error=%v released=%v failures=%+v", err, revisions.released, run.files)
	}
}

func TestAtomicRequestedRangeSinkCancellationAndTerminalStateFailClosed(t *testing.T) {
	var nilSink *atomicRequestedRangeSink
	if err := nilSink.Flush(context.Background()); !errors.Is(err, errRangeReaderContract) || !isJobFatal(err) {
		t.Fatalf("nil atomic sink flush error = %v", err)
	}
	if err := nilSink.Failure(); !errors.Is(err, errRangeReaderContract) || !isJobFatal(err) {
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
	if err := postFlush.WriteRange(context.Background(), 30, []byte{1, 2}); !errors.Is(err, errRangeReaderContract) || !isJobFatal(err) {
		t.Fatalf("post-flush atomic write error = %v", err)
	}
	if err := sealed.Flush(context.Background()); !errors.Is(err, errRangeReaderContract) || !isJobFatal(err) {
		t.Fatalf("duplicate atomic flush error = %v", err)
	}
	if err := sealed.WriteRange(context.Background(), 20, []byte{1, 2}); !errors.Is(err, errRangeReaderContract) {
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
		if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrOutputContract) ||
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
			failures: make(map[catalog.FileID]error), releaseErr: NewSessionFailure(cause),
		}
		run := immediateSettlementJobRun(share, revisions)
		plan := plannedFile{file: selected, path: "file.bin", entry: jobEntry(t, selected, "file.bin", 1)}
		_, ready, err := run.openSelectedRevision(context.Background(), plan)
		if ready || !errors.Is(err, cause) || len(revisions.released) != 1 || revisions.released[0] != lease {
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

func immediateJobSettlements(
	t *testing.T,
	binding OutputFileBinding,
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
	reference, err := NewOutputStateRef(binding.OutputSessionID(), binding.Locator().Digest())
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
