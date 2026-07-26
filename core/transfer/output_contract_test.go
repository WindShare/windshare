package transfer

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestCanonicalSelectionCombinesNormalizedRequestAndTerminalPlan(t *testing.T) {
	share := transferID[catalog.ShareInstance](1)
	root := transferID[catalog.DirectoryID](2)
	directoryA := transferID[catalog.DirectoryID](3)
	directoryB := transferID[catalog.DirectoryID](4)
	file := transferID[catalog.FileID](5)
	rulesA, err := NewSelectionRules(false, []SelectionOverride{
		{FileID: file, Selected: true, Ancestors: []catalog.DirectoryID{root, directoryA}},
		{DirectoryID: directoryB, Selected: false, Ancestors: []catalog.DirectoryID{root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := NewSelectionRules(false, []SelectionOverride{
		{DirectoryID: directoryB, Selected: false}, {FileID: file, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestA, err := NewCanonicalSelectionRequest(share, root, rulesA)
	if err != nil {
		t.Fatal(err)
	}
	requestB, err := NewCanonicalSelectionRequest(share, root, rulesB)
	if err != nil || !slices.Equal(requestA.Bytes(), requestB.Bytes()) {
		t.Fatalf("normalized requests differ: err=%v", err)
	}

	modified, _ := catalog.NewModifiedTime(1_700_000_000, 123_000_000, catalog.TimePrecisionMilliseconds)
	rootGeneration := transferID[catalog.DirectoryGeneration](6)
	directoryGeneration := transferID[catalog.DirectoryGeneration](7)
	plan, err := NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]OutputSelectionDirectory{{
			Path: "folder", DirectoryID: directoryA, Generation: directoryGeneration, ModifiedTime: modified,
		}},
		[]OutputSelectionFile{{
			Path: "folder/file.bin", FileID: file, ParentDirectoryID: directoryA,
			ParentGeneration: directoryGeneration, ExpectedSize: 42, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	canonicalA, err := NewCanonicalSelectionV1(requestA, plan)
	if err != nil {
		t.Fatal(err)
	}
	canonicalB, err := NewCanonicalSelectionV1(requestB, plan)
	if err != nil || canonicalA.ResumeIntent() != canonicalB.ResumeIntent() {
		t.Fatalf("equivalent selections produced different intents: err=%v", err)
	}
	bound, err := canonicalA.BindPlan(plan)
	if err != nil || bound.ResumeIntent() != canonicalA.ResumeIntent() || bound.Identity() != plan.Identity() ||
		bound.CanonicalSelection().ResumeIntent() != canonicalA.ResumeIntent() {
		t.Fatalf("bound plan mismatch: err=%v", err)
	}
	differentRules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	differentRequest, err := NewCanonicalSelectionRequest(share, root, differentRules)
	if err != nil {
		t.Fatal(err)
	}
	differentCanonical, err := NewCanonicalSelectionV1(differentRequest, plan)
	if err != nil || differentCanonical.ResumeIntent() == canonicalA.ResumeIntent() {
		t.Fatalf("different request semantics reused resume intent: err=%v", err)
	}

	changedTime, _ := catalog.NewModifiedTime(1_700_000_001, 0, catalog.TimePrecisionSeconds)
	changedPlan, err := NewOutputSelection(
		share,
		root,
		rootGeneration,
		plan.Directories(),
		[]OutputSelectionFile{{
			Path: "folder/file.bin", FileID: file, ParentDirectoryID: directoryA,
			ParentGeneration: directoryGeneration, ExpectedSize: 42, ModifiedTime: changedTime,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	changedCanonical, err := NewCanonicalSelectionV1(requestA, changedPlan)
	if err != nil || changedCanonical.ResumeIntent() == canonicalA.ResumeIntent() {
		t.Fatalf("catalog metadata change preserved resume intent: err=%v", err)
	}
	if _, err := canonicalA.BindPlan(changedPlan); !errors.Is(err, ErrInvalidOutputSelection) {
		t.Fatalf("canonical selection rebound to a different plan: %v", err)
	}
}

func TestFileStartAndSettlementPayloadsAreChecked(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	backend, _ := NewOutputBackendID("test/settlement")
	session := transferID[OutputSessionID](8)
	locator, _ := NewPathOutputLocator("file.bin")
	var object OutputObjectIdentity
	object[0] = 9
	binding, err := NewOutputFileBinding(backend, session, descriptor, locator, object)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Target().Descriptor() != descriptor {
		t.Fatal("output target did not retain the complete revision descriptor")
	}
	if _, err := NewCollisionFileSettlement(OutputFileTarget{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound collision error=%v", err)
	}
	if _, err := NewRetiredFileSettlement(OutputFileBinding{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound retirement error=%v", err)
	}
	partialRanges, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 1}})
	partial, _ := VerifyDurableRanges(binding, 1, partialRanges)
	if _, err := NewVerifiedFileSettlement(FilePublished, partial); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("partial published settlement error=%v", err)
	}
	paused, err := NewVerifiedFileSettlement(FilePaused, partial)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint, ok := paused.VerifiedCheckpoint(); !ok || checkpoint.CheckpointGeneration() != 1 {
		t.Fatalf("paused checkpoint=(%+v,%v)", checkpoint, ok)
	}
	if _, err := NewFileSettlementStart(paused); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("paused immediate start error=%v", err)
	}
	collision, _ := NewCollisionFileSettlement(binding.Target())
	start, err := NewFileSettlementStart(collision)
	if settlement, ok := start.ImmediateSettlement(); err != nil || !ok || settlement.Kind() != FileCollision {
		t.Fatalf("collision start=(%+v,%v) err=%v", settlement, ok, err)
	}
	retired, _ := NewRetiredFileSettlement(binding)
	retiredStart, err := NewFileSettlementStart(retired)
	retiredSettlement, retiredImmediate := retiredStart.ImmediateSettlement()
	if err != nil || !retiredImmediate || retiredSettlement.Kind() != FileRetired {
		t.Fatalf("retired immediate start=(%+v,%v) error=%v", retiredSettlement, retiredImmediate, err)
	}
	if transaction, _, ok := retiredStart.Transaction(); ok || transaction != nil {
		t.Fatal("retired immediate start exposed a second file transaction")
	}
	otherObject := object
	otherObject[0]++
	otherBinding, err := BindOutputFileTarget(binding.Target(), otherObject)
	if err != nil {
		t.Fatal(err)
	}
	if retired.matchesBinding(otherBinding) {
		t.Fatal("retirement settlement matched a different owned object")
	}
	reference, _ := NewOutputStateRef(session, locator.Digest())
	quarantined, err := NewImmediateQuarantinedFileSettlement(
		binding.Target(), reference, QuarantineOwnershipMismatch,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotRef, reason, ok := quarantined.Quarantine(); !ok || gotRef != reference || reason != QuarantineOwnershipMismatch {
		t.Fatalf("quarantine payload=(%+v,%v,%v)", gotRef, reason, ok)
	}
	transactionQuarantine, err := NewTransactionQuarantinedFileSettlement(
		binding, reference, QuarantineOwnershipMismatch,
	)
	if err != nil || transactionQuarantine.matchesBinding(otherBinding) {
		t.Fatalf("transaction quarantine accepted foreign binding: err=%v", err)
	}

	cause := errors.New("state write")
	fault := NewOutputFault(OutputFaultSession, OutputFaultStateIO, cause)
	var typed *OutputFault
	if !errors.As(fault, &typed) || typed.Scope() != OutputFaultSession || typed.Code() != OutputFaultStateIO ||
		!errors.Is(fault, cause) {
		t.Fatalf("fault=%v typed=%+v", fault, typed)
	}
}

func TestTransferJobAdmitsFrozenSelectionBeforeOpeningRevision(t *testing.T) {
	share := transferID[catalog.ShareInstance](20)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	job, _ := branchJob(t, output, revisions, scriptedRangeReader{})
	revisions.openHook = func() {
		output.mu.Lock()
		defer output.mu.Unlock()
		if len(output.admitted) != 1 || output.admitted[0].ResumeIntent().IsZero() {
			t.Error("revision opened before terminal selection admission")
		}
	}
	result := job.Run(context.Background())
	if result.Outcome != JobSucceeded || result.ResumeIntent.IsZero() || result.SelectionIdentity.IsZero() {
		t.Fatalf("result=%+v", result)
	}

	output = newJobOutput(share)
	unsupported := errors.New("unsupported filesystem")
	output.admitErr = NewOutputFault(OutputFaultRoot, OutputFaultUnsupportedFilesystem, unsupported)
	revisions = &jobRevisionClient{}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{})
	result = job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, unsupported) ||
		len(revisions.order) != 0 || len(output.transactions) != 0 || output.pauseCalls != 0 || output.completeCalls != 0 {
		t.Fatalf("failed admission leaked revision/content work: result=%+v revisions=%v", result, revisions.order)
	}
}

func TestTransferJobPreparesEveryDirectoryBeforeAdmissionAndContent(t *testing.T) {
	share := transferID[catalog.ShareInstance](21)
	root := transferID[catalog.DirectoryID](22)
	folder := transferID[catalog.DirectoryID](23)
	other := transferID[catalog.DirectoryID](27)
	file := transferID[catalog.FileID](24)
	descriptor := jobDescriptor(t, share, file, 25, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](26), descriptor)
	rules, _ := NewSelectionRules(true, nil)
	newJob := func(output *jobOutput, revisions *jobRevisionClient, blocks RangeReader) *TransferJob {
		job, err := NewTransferJob(TransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root: jobSnapshot(t, share, root, 1,
						jobDirectoryEntry(t, folder, "folder"), jobDirectoryEntry(t, other, "other"),
					),
					folder: jobSnapshot(t, share, folder, 2, jobEntry(t, file, "file.bin", 1)),
					other:  jobSnapshot(t, share, other, 3),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: revisions, Blocks: blocks, Output: output,
		})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}

	t.Run("successful ordering", func(t *testing.T) {
		output := newJobOutput(share)
		revisions := &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		}
		revisions.openHook = func() {
			output.mu.Lock()
			defer output.mu.Unlock()
			if !slices.Equal(output.events, []string{"ensure:folder", "ensure:other", "open"}) {
				t.Errorf("revision opened after events %v", output.events)
			}
		}
		result := newJob(output, revisions, scriptedRangeReader{}).Run(context.Background())
		if result.Outcome != JobSucceeded || !slices.Equal(output.events, []string{
			"ensure:folder", "ensure:other", "open", "begin:folder/file.bin", "complete",
		}) {
			t.Fatalf("result=%+v events=%v", result, output.events)
		}
	})

	t.Run("one failed directory prevents partial admission", func(t *testing.T) {
		output := newJobOutput(share)
		output.ensureFailures = map[string]error{"folder": errors.New("parent unavailable")}
		revisions := &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		}
		blocks := &countingRangeReader{}
		result := newJob(output, revisions, blocks).Run(context.Background())
		if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, output.ensureFailures["folder"]) ||
			len(output.admitted) != 0 || len(revisions.order) != 0 || blocks.calls != 0 ||
			output.pauseCalls != 0 || output.completeCalls != 0 ||
			!slices.Equal(output.events, []string{"ensure:folder", "ensure:other"}) {
			t.Fatalf("result=%+v events=%v revisions=%v blocks=%d", result, output.events, revisions.order, blocks.calls)
		}
	})
}

type countingRangeReader struct{ calls int }

func (reader *countingRangeReader) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	reader.calls++
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func TestTransferJobRejectsForeignOrMissingImmediateSettlementAuthority(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, *jobOutput, content.FileRevisionDescriptor)
	}{
		{
			name: "foreign collision revision",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				wrongDescriptor, err := content.NewFileRevisionDescriptor(
					descriptor.ShareInstance(), descriptor.FileID(), transferID[content.FileRevision](203),
					descriptor.Geometry(), descriptor.ModifiedTime(),
				)
				if err != nil {
					t.Fatal(err)
				}
				locator, err := NewPathOutputLocator("file.bin")
				if err != nil {
					t.Fatal(err)
				}
				target, err := NewOutputFileTarget(
					output.BackendID(), output.SessionID(), wrongDescriptor, locator,
				)
				if err != nil {
					t.Fatal(err)
				}
				settlement, err := NewCollisionFileSettlement(target)
				if err != nil {
					t.Fatal(err)
				}
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "foreign published session",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				settlement := immediatePublishedSettlement(t, output, descriptor, "file.bin", transferID[OutputSessionID](201))
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "noncanonical published locator",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				settlement := immediatePublishedSettlement(t, output, descriptor, "other.bin", output.session)
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "foreign quarantine reference",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				locator, err := NewPathOutputLocator("file.bin")
				if err != nil {
					t.Fatal(err)
				}
				foreignSession := transferID[OutputSessionID](202)
				target, err := NewOutputFileTarget(output.BackendID(), foreignSession, descriptor, locator)
				if err != nil {
					t.Fatal(err)
				}
				reference, err := NewOutputStateRef(foreignSession, locator.Digest())
				if err != nil {
					t.Fatal(err)
				}
				settlement, err := NewImmediateQuarantinedFileSettlement(
					target, reference, QuarantineOwnershipMismatch,
				)
				if err != nil {
					t.Fatal(err)
				}
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "published settlement without checkpoint binding",
			configure: func(_ *testing.T, output *jobOutput, _ content.FileRevisionDescriptor) {
				start := FileStart{kind: fileStartSettlement, settlement: FileSettlement{kind: FilePublished}}
				output.rawStart = &start
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](80)
			output := newJobOutput(share)
			revisions := &jobRevisionClient{}
			blocks := &countingRangeReader{}
			job, file := branchJob(t, output, revisions, blocks)
			test.configure(t, output, revisions.opened[file].Descriptor)

			result := job.Run(context.Background())
			var fault *OutputFault
			if result.Outcome != JobPausedOutcome || result.SucceededFiles != 0 || blocks.calls != 0 ||
				len(result.Files) != 1 || !errors.As(result.SettlementFailure, &fault) ||
				fault.Scope() != OutputFaultSession || fault.Code() != OutputFaultContract ||
				output.pauseCalls != 1 || output.completeCalls != 0 {
				t.Fatalf("result=%+v fault=%+v blocks=%d pause=%d complete=%d", result, fault, blocks.calls, output.pauseCalls, output.completeCalls)
			}
		})
	}
}

func immediatePublishedSettlement(
	t *testing.T,
	output *jobOutput,
	descriptor content.FileRevisionDescriptor,
	path string,
	session OutputSessionID,
) FileSettlement {
	t.Helper()
	locator, err := NewPathOutputLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	var object OutputObjectIdentity
	object[0] = 1
	binding, err := NewOutputFileBinding(output.BackendID(), session, descriptor, locator, object)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize()}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := VerifyDurableRanges(binding, 1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := NewVerifiedFileSettlement(FilePublished, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return settlement
}

func TestTransferJobIsolatesImmediateCollisionAndQuarantine(t *testing.T) {
	for _, test := range []struct {
		name       string
		settlement func(*jobOutput, string, content.FileRevisionDescriptor) FileSettlement
		cause      error
	}{
		{
			name: "collision",
			settlement: func(output *jobOutput, path string, descriptor content.FileRevisionDescriptor) FileSettlement {
				locator, _ := NewPathOutputLocator(path)
				target, _ := NewOutputFileTarget(output.BackendID(), output.SessionID(), descriptor, locator)
				settlement, _ := NewCollisionFileSettlement(target)
				return settlement
			},
			cause: ErrOutputPublishBlocked,
		},
		{
			name: "quarantine",
			settlement: func(output *jobOutput, path string, descriptor content.FileRevisionDescriptor) FileSettlement {
				locator, _ := NewPathOutputLocator(path)
				target, _ := NewOutputFileTarget(output.BackendID(), output.SessionID(), descriptor, locator)
				reference, _ := NewOutputStateRef(output.session, locator.Digest())
				settlement, _ := NewImmediateQuarantinedFileSettlement(
					target, reference, QuarantineOwnershipMismatch,
				)
				return settlement
			},
			cause: ErrOutputQuarantined,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](30)
			root := transferID[catalog.DirectoryID](31)
			blockedFile := transferID[catalog.FileID](32)
			goodFile := transferID[catalog.FileID](33)
			blockedPath := "blocked.bin"
			snapshot := jobSnapshot(t, share, root, 34,
				jobEntry(t, blockedFile, blockedPath, 0), jobEntry(t, goodFile, "good.bin", 0),
			)
			revisions := &jobRevisionClient{
				opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
			}
			for index, file := range []catalog.FileID{blockedFile, goodFile} {
				descriptor := jobDescriptor(t, share, file, byte(40+index), 0)
				revisions.opened[file], _ = NewOpenedRevision(
					transferID[content.LeaseID](byte(50+index)), descriptor,
				)
			}
			output := newJobOutput(share)
			output.immediate[blockedPath] = test.settlement(
				output, blockedPath, revisions.opened[blockedFile].Descriptor,
			)
			output.completeSettlement = JobPausedNeedsAttention
			rules, _ := NewSelectionRules(true, nil)
			job, err := NewTransferJob(TransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot},
					failures:  make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Output: output,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(context.Background())
			if result.Outcome != JobCompletedWithErrors || result.Settlement.Kind() != JobPausedNeedsAttention ||
				result.SucceededFiles != 1 || len(result.Files) != 1 || !errors.Is(result.Files[0].Cause, test.cause) {
				t.Fatalf("result=%+v", result)
			}
			if _, exists := output.transactions[blockedPath]; exists {
				t.Fatal("immediate settlement exposed a transaction")
			}
			if transaction := output.transactions["good.bin"]; transaction == nil || !transaction.committed {
				t.Fatal("isolated settlement prevented the independent file commit")
			}
		})
	}
}

func TestTransferJobRejectsClosedSessionWithQuarantinedState(t *testing.T) {
	share := transferID[catalog.ShareInstance](59)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	job, file := branchJob(t, output, revisions, scriptedRangeReader{})
	descriptor := revisions.opened[file].Descriptor
	locator, _ := NewPathOutputLocator("file.bin")
	target, _ := NewOutputFileTarget(output.BackendID(), output.SessionID(), descriptor, locator)
	reference, _ := NewOutputStateRef(output.SessionID(), locator.Digest())
	quarantined, _ := NewImmediateQuarantinedFileSettlement(
		target, reference, QuarantineOwnershipMismatch,
	)
	output.immediate["file.bin"] = quarantined

	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || len(result.Files) != 1 ||
		!errors.Is(result.Files[0].Cause, ErrOutputQuarantined) ||
		!errors.Is(result.SettlementFailure, ErrOutputContract) ||
		output.completeCalls != 1 || output.pauseCalls != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobSettlementContextAndFailuresRemainIndependent(t *testing.T) {
	share := transferID[catalog.ShareInstance](60)
	output := newJobOutput(share)
	settlementCause := errors.New("checkpoint state unavailable")
	settlementObserved := false
	output.transactionScript.pauseHook = func(ctx context.Context, reason FilePauseReason) {
		settlementObserved = true
		if ctx.Err() != nil || reason != FilePauseSessionFailure {
			t.Errorf("settlement context/reason err=%v reason=%v", ctx.Err(), reason)
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > DefaultOutputSettlementTimeout {
			t.Errorf("settlement deadline=%v bounded=%v", deadline, bounded)
		}
	}
	output.transactionScript.pauseErr = NewOutputFault(OutputFaultFile, OutputFaultStateIO, settlementCause)
	originalCause := errors.New("authenticated session ended")
	revisions := &jobRevisionClient{}
	job, _ := branchJob(t, output, revisions, sessionFailingBlocks{err: NewSessionFailure(originalCause)})
	result := job.Run(context.Background())
	if !settlementObserved || result.Outcome != JobPausedOutcome ||
		!errors.Is(result.TerminationCause, originalCause) || errors.Is(result.TerminationCause, settlementCause) ||
		!errors.Is(result.SettlementFailure, settlementCause) || len(result.Files) != 1 ||
		!errors.Is(result.Files[0].SettlementFailure, settlementCause) {
		t.Fatalf("result=%+v observed=%v", result, settlementObserved)
	}
}

func TestTransferJobTerminalSettlementMethodsAreSingleShot(t *testing.T) {
	t.Run("commit failure delegates recovery to job pause", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](91)
		output := newJobOutput(share)
		commitCause := errors.New("publication state could not be verified")
		output.transactionScript.commitErr = NewOutputFault(
			OutputFaultFile, OutputFaultStateIO, commitCause,
		)
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		if result.Outcome != JobPausedOutcome || result.TerminationCause != nil ||
			!errors.Is(result.SettlementFailure, commitCause) || transaction == nil ||
			transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
			len(transaction.retireReasons) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
			t.Fatalf("result=%+v transaction=%+v", result, transaction)
		}
	})

	t.Run("invalid commit result is not followed by file pause", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](92)
		output := newJobOutput(share)
		revisions := &jobRevisionClient{}
		job, file := branchJob(t, output, revisions, scriptedRangeReader{})
		descriptor := revisions.opened[file].Descriptor
		locator, _ := NewPathOutputLocator("file.bin")
		target, _ := NewOutputFileTarget(output.BackendID(), output.SessionID(), descriptor, locator)
		collision, _ := NewCollisionFileSettlement(target)
		output.transactionScript.commitResult = &collision

		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrOutputContract) ||
			!errors.Is(result.SettlementFailure, ErrOutputContract) || transaction == nil ||
			transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
			len(transaction.retireReasons) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
			t.Fatalf("result=%+v transaction=%+v", result, transaction)
		}
	})

	t.Run("complete failure is not followed by job pause", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](93)
		output := newJobOutput(share)
		completeCause := errors.New("session cleanup could not be verified")
		output.finishErr = NewOutputFault(OutputFaultSession, OutputFaultStateIO, completeCause)
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		if result.Outcome != JobPausedOutcome || result.TerminationCause != nil ||
			!errors.Is(result.SettlementFailure, completeCause) || transaction == nil ||
			transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
			output.completeCalls != 1 || output.pauseCalls != 0 {
			t.Fatalf("result=%+v transaction=%+v", result, transaction)
		}
	})
}

func TestTransferLifecycleTraceCarriesStableTypedMilestones(t *testing.T) {
	share := transferID[catalog.ShareInstance](70)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	var traces []TransferLifecycleTrace
	job, file := branchJob(t, output, revisions, scriptedRangeReader{})
	job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		traces = append(traces, event)
	})
	result := job.Run(context.Background())
	if result.Outcome != JobSucceeded {
		t.Fatalf("result=%+v", result)
	}
	wantStages := []TransferLifecycleStage{
		TransferDiscoveryStarted, TransferDiscoveryCompleted, TransferSelectionFrozen,
		TransferAdmissionStarted, TransferAdmissionCompleted, TransferFileStarted,
		TransferFileSettled, TransferJobSettled,
	}
	if len(traces) != len(wantStages) {
		t.Fatalf("traces=%+v", traces)
	}
	for index, trace := range traces {
		if trace.Stage != wantStages[index] {
			t.Fatalf("trace[%d]=%+v", index, trace)
		}
		if index < 4 && !trace.OutputSessionID.IsZero() || index >= 4 && trace.OutputSessionID != output.session {
			t.Fatalf("trace[%d] session=%x", index, trace.OutputSessionID)
		}
		if index >= 1 && (trace.ResumeIntent.IsZero() || trace.SelectionIdentity.IsZero() && index < 5) {
			t.Fatalf("uncorrelated trace[%d]=%+v", index, trace)
		}
	}
	if traces[5].FileID != file || traces[6].FileSettlement != FilePublished ||
		traces[7].JobSettlement != JobClosed {
		t.Fatalf("terminal traces=%+v", traces[5:])
	}
}
