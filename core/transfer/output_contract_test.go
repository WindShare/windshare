package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestFileStartAndSettlementPayloadsAreChecked(t *testing.T) {
	descriptor := transferDescriptor(t, 1)
	session := transferID[OutputSessionID](8)
	locator, _ := NewPathMaterializationLocator("file.bin")
	var object OwnedObjectID
	object[0] = 9
	binding, err := NewMaterializedFileBinding(session, descriptor, locator, object)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Target().Descriptor() != descriptor {
		t.Fatal("output target did not retain the complete revision descriptor")
	}
	if _, err := NewCollisionFileSettlement(FileMaterializationTarget{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound collision error=%v", err)
	}
	if _, err := NewFailedFileSettlement(MaterializedFileBinding{}); !errors.Is(err, ErrInvalidOutputSettlement) {
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
	retired, _ := NewFailedFileSettlement(binding)
	retiredStart, err := NewFileSettlementStart(retired)
	retiredSettlement, retiredImmediate := retiredStart.ImmediateSettlement()
	if err != nil || !retiredImmediate || retiredSettlement.Kind() != FileFailed {
		t.Fatalf("retired immediate start=(%+v,%v) error=%v", retiredSettlement, retiredImmediate, err)
	}
	if transaction, _, ok := retiredStart.Transaction(); ok || transaction != nil {
		t.Fatal("retired immediate start exposed a second file transaction")
	}
	otherObject := object
	otherObject[0]++
	otherBinding, err := BindFileMaterializationTarget(binding.Target(), otherObject)
	if err != nil {
		t.Fatal(err)
	}
	if retired.matchesBinding(otherBinding) {
		t.Fatal("retirement settlement matched a different owned object")
	}
	reference, _ := NewMaterializationStateRef(session, locator.Digest())
	quarantined, err := NewImmediateItemBlockedFileSettlement(
		binding.Target(), reference, ItemBlockOwnershipUnknown,
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotRef, reason, ok := quarantined.ItemBlock(); !ok || gotRef != reference || reason != ItemBlockOwnershipUnknown {
		t.Fatalf("quarantine payload=(%+v,%v,%v)", gotRef, reason, ok)
	}
	transactionQuarantine, err := NewTransactionItemBlockedFileSettlement(
		binding, reference, ItemBlockOwnershipUnknown,
	)
	if err != nil || transactionQuarantine.matchesBinding(otherBinding) {
		t.Fatalf("transaction quarantine accepted foreign binding: err=%v", err)
	}

	cause := errors.New("state write")
	value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
	boundary := fault.Wrap(value, cause)
	var typed *fault.BoundaryError
	if !errors.As(boundary, &typed) || typed.Fault() != value || !errors.Is(boundary, cause) {
		t.Fatalf("fault=%v typed=%+v", boundary, typed)
	}
}

func TestTransferJobOpensOutputBeforeRevisionAndAdmitsGeneration(t *testing.T) {
	share := transferID[catalog.ShareInstance](20)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	job, _ := branchJob(t, output, revisions, scriptedRangeReader{})
	revisions.openHook = func() {
		output.mu.Lock()
		defer output.mu.Unlock()
		if output.intent.IsZero() || !slices.Equal(output.directories, []string{""}) || output.events[0] != "open" {
			t.Errorf("revision opened before output admission: intent=%v directories=%v events=%v", !output.intent.IsZero(), output.directories, output.events)
		}
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.ReceiveIntentDigest.IsZero() || result.ReceiveIntent.IsZero() || result.SucceededFiles != 1 {
		t.Fatalf("result=%+v", result)
	}

	output = newJobOutput(share)
	unsupported := errors.New("unsupported filesystem")
	output.admitErr = outputFailure(fault.ScopeOutputPause, fault.OutputUnsupportedFilesystem, unsupported)
	revisions = &jobRevisionClient{}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{})
	result = job.Run(context.Background())
	expectedUnsupported, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputUnsupportedFilesystem)
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != expectedUnsupported ||
		len(revisions.order) != 0 || len(output.transactions) != 0 || output.pauseCalls != 0 || output.completeCalls != 0 {
		t.Fatalf("failed admission leaked revision/content work: result=%+v revisions=%v", result, revisions.order)
	}

	output = newJobOutput(share)
	output.admitErr = outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("root opened before writer failed"))
	output.returnSessionOnOpenError = true
	revisions = &jobRevisionClient{}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{})
	result = job.Run(context.Background())
	expectedStateIO, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != expectedStateIO ||
		result.Settlement.Kind() != DirectTreeSettlementPaused || output.pauseCalls != 1 ||
		output.completeCalls != 0 || len(revisions.order) != 0 {
		t.Fatalf("bound partial admission was not paused: result=%+v pause=%d", result, output.pauseCalls)
	}
}

func TestTransferJobAdmitsGenerationsIncrementallyBeforeContent(t *testing.T) {
	share := transferID[catalog.ShareInstance](21)
	root := transferID[catalog.DirectoryID](22)
	folder := transferID[catalog.DirectoryID](23)
	other := transferID[catalog.DirectoryID](27)
	file := transferID[catalog.FileID](24)
	descriptor := jobDescriptor(t, share, file, 25, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](26), descriptor)
	rules, _ := NewSelectionRules(true, nil)
	newJob := func(output *jobOutput, revisions *jobRevisionClient, blocks RangeReader) *TransferJob {
		job, err := newTestTransferJob(t, testTransferJobConfig{
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
			Revisions: revisions, Blocks: blocks, Materializer: output,
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
			if len(output.events) == 0 || output.events[0] != "open" ||
				!slices.Contains(output.directories, "") || !slices.Contains(output.directories, "folder") {
				t.Errorf("revision opened without incremental output admission: events=%v directories=%v", output.events, output.directories)
			}
		}
		result := newJob(output, revisions, scriptedRangeReader{}).Run(context.Background())
		if result.Outcome != DirectTreeOutcomeSuccess || !slices.Equal(output.directories, []string{"", "folder", "other"}) ||
			!slices.Equal(output.finalized, []string{"folder", "other", ""}) ||
			!slices.Contains(output.events, "begin:folder/file.bin") || output.completeCalls != 1 {
			t.Fatalf("result=%+v events=%v", result, output.events)
		}
	})

	t.Run("one failed directory prevents partial admission", func(t *testing.T) {
		output := newJobOutput(share)
		output.ensureFailures = map[string]error{
			"folder": outputFailure(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata, errors.New("parent unavailable")),
		}
		revisions := &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		}
		blocks := &countingRangeReader{}
		result := newJob(output, revisions, blocks).Run(context.Background())
		if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil || len(result.Directories) != 1 ||
			result.Directories[0].Stage != FailureDirectoryOutput ||
			len(revisions.order) != 0 || blocks.calls != 0 || output.pauseCalls != 0 || output.completeCalls != 1 ||
			!slices.Equal(output.directories, []string{"", "other"}) ||
			!slices.Equal(output.finalized, []string{"other", ""}) {
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
				locator, err := NewPathMaterializationLocator("file.bin")
				if err != nil {
					t.Fatal(err)
				}
				target, err := NewFileMaterializationTarget(
					output.SessionID(), wrongDescriptor, locator,
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
				settlement := immediatePublishedSettlement(t, descriptor, "file.bin", transferID[OutputSessionID](201))
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "noncanonical published locator",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				settlement := immediatePublishedSettlement(t, descriptor, "other.bin", output.session)
				output.immediate["file.bin"] = settlement
			},
		},
		{
			name: "foreign quarantine reference",
			configure: func(t *testing.T, output *jobOutput, descriptor content.FileRevisionDescriptor) {
				locator, err := NewPathMaterializationLocator("file.bin")
				if err != nil {
					t.Fatal(err)
				}
				foreignSession := transferID[OutputSessionID](202)
				target, err := NewFileMaterializationTarget(foreignSession, descriptor, locator)
				if err != nil {
					t.Fatal(err)
				}
				reference, err := NewMaterializationStateRef(foreignSession, locator.Digest())
				if err != nil {
					t.Fatal(err)
				}
				settlement, err := NewImmediateItemBlockedFileSettlement(
					target, reference, ItemBlockOwnershipUnknown,
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
			expected := mustOutputFault(fault.ScopeOutputPause, fault.OutputContract)
			if result.Outcome != DirectTreeOutcomePaused || result.SucceededFiles != 0 || blocks.calls != 0 ||
				len(result.Files) != 1 || result.SettlementFault != expected ||
				output.pauseCalls != 1 || output.completeCalls != 0 {
				t.Fatalf("result=%+v blocks=%d pause=%d complete=%d", result, blocks.calls, output.pauseCalls, output.completeCalls)
			}
		})
	}
}

func immediatePublishedSettlement(
	t *testing.T,
	descriptor content.FileRevisionDescriptor,
	path string,
	session OutputSessionID,
) FileSettlement {
	t.Helper()
	locator, err := NewPathMaterializationLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	var object OwnedObjectID
	object[0] = 1
	binding, err := NewMaterializedFileBinding(session, descriptor, locator, object)
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

func TestTransferJobIsolatesImmediateCollisionAndItemBlocks(t *testing.T) {
	itemBlockedSettlement := func(reason ItemBlockReason) func(*jobOutput, string, content.FileRevisionDescriptor) FileSettlement {
		return func(output *jobOutput, path string, descriptor content.FileRevisionDescriptor) FileSettlement {
			locator, _ := NewPathMaterializationLocator(path)
			target, _ := NewFileMaterializationTarget(output.SessionID(), descriptor, locator)
			reference, _ := NewMaterializationStateRef(output.session, locator.Digest())
			settlement, _ := NewImmediateItemBlockedFileSettlement(target, reference, reason)
			return settlement
		}
	}
	for _, test := range []struct {
		name       string
		settlement func(*jobOutput, string, content.FileRevisionDescriptor) FileSettlement
		cause      error
		outcomes   FileOutcomeSummary
	}{
		{
			name: "collision",
			settlement: func(output *jobOutput, path string, descriptor content.FileRevisionDescriptor) FileSettlement {
				locator, _ := NewPathMaterializationLocator(path)
				target, _ := NewFileMaterializationTarget(output.SessionID(), descriptor, locator)
				settlement, _ := NewCollisionFileSettlement(target)
				return settlement
			},
			cause:    ErrOutputPublishBlocked,
			outcomes: FileOutcomeSummary{DownloadedFiles: 1, CollisionFiles: 1},
		},
		{
			name:       "quarantine",
			settlement: itemBlockedSettlement(ItemBlockOwnershipUnknown),
			cause:      ErrOutputQuarantined,
			outcomes:   FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1},
		},
		{
			name:       "revision conflict",
			settlement: itemBlockedSettlement(ItemBlockRevisionConflict),
			cause:      ErrOutputQuarantined,
			outcomes: FileOutcomeSummary{
				DownloadedFiles: 1, ItemBlockedFiles: 1, RevisionConflictFiles: 1,
			},
		},
		{
			name:       "invalid checkpoint",
			settlement: itemBlockedSettlement(ItemBlockCheckpointInvalid),
			cause:      ErrOutputQuarantined,
			outcomes: FileOutcomeSummary{
				DownloadedFiles: 1, ItemBlockedFiles: 1, CheckpointInvalidFiles: 1,
			},
		},
		{
			name:       "owned object conflict",
			settlement: itemBlockedSettlement(ItemBlockOwnedObjectUnknown),
			cause:      ErrOutputQuarantined,
			outcomes: FileOutcomeSummary{
				DownloadedFiles: 1, ItemBlockedFiles: 1, OwnedObjectUnknownFiles: 1,
			},
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
			output.completeSettlement = DirectTreeSettlementFailed
			rules, _ := NewSelectionRules(true, nil)
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot},
					failures:  make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(context.Background())
			if result.Outcome != DirectTreeOutcomeFailed || result.Settlement.Kind() != DirectTreeSettlementFailed ||
				result.SucceededFiles != 1 || len(result.Files) != 1 || !errors.Is(result.Files[0].Cause, test.cause) ||
				result.Progress.FileOutcomes != test.outcomes || !result.Progress.CountersExact {
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

func TestTransferJobKeepsItemBlockedOperationActive(t *testing.T) {
	share := transferID[catalog.ShareInstance](59)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	job, file := branchJob(t, output, revisions, scriptedRangeReader{})
	descriptor := revisions.opened[file].Descriptor
	locator, _ := NewPathMaterializationLocator("file.bin")
	target, _ := NewFileMaterializationTarget(output.SessionID(), descriptor, locator)
	reference, _ := NewMaterializationStateRef(output.SessionID(), locator.Digest())
	quarantined, _ := NewImmediateItemBlockedFileSettlement(
		target, reference, ItemBlockOwnershipUnknown,
	)
	output.immediate["file.bin"] = quarantined

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || len(result.Files) != 1 ||
		!errors.Is(result.Files[0].Cause, ErrOutputQuarantined) ||
		result.SettlementFailure != nil ||
		output.completeCalls != 1 || output.pauseCalls != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferDirectTreeSettlementContextAndFailuresRemainIndependent(t *testing.T) {
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
	output.transactionScript.pauseErr = outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, settlementCause)
	originalCause := errors.New("authenticated session ended")
	revisions := &jobRevisionClient{}
	job, _ := branchJob(t, output, revisions, sessionFailingBlocks{err: sessionProtocolFailure(originalCause)})
	result := job.Run(context.Background())
	sessionFault := mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol)
	settlementFault, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputStateIO)
	if !settlementObserved || result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != sessionFault || result.SettlementFault != settlementFault ||
		len(result.Files) != 1 || result.Files[0].SettlementFault != settlementFault {
		t.Fatalf("result=%+v observed=%v", result, settlementObserved)
	}
}

func TestTransferJobTerminalSettlementMethodsAreSingleShot(t *testing.T) {
	t.Run("commit failure stops work and delegates recovery to job pause", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](91)
		output := newJobOutput(share)
		commitCause := errors.New("publication state could not be verified")
		output.transactionScript.commitErr = outputFailure(
			fault.ScopeFileLocal, fault.OutputStateIO, commitCause,
		)
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		expected, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputStateIO)
		if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != expected ||
			result.SettlementFault != expected || transaction == nil ||
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
		locator, _ := NewPathMaterializationLocator("file.bin")
		target, _ := NewFileMaterializationTarget(output.SessionID(), descriptor, locator)
		collision, _ := NewCollisionFileSettlement(target)
		output.transactionScript.commitResult = &collision

		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
			result.SettlementFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) || transaction == nil ||
			transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
			len(transaction.retireReasons) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
			t.Fatalf("result=%+v transaction=%+v", result, transaction)
		}
	})

	t.Run("complete failure is not followed by job pause", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](93)
		output := newJobOutput(share)
		completeCause := errors.New("session cleanup could not be verified")
		output.finishErr = outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, completeCause)
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
		result := job.Run(context.Background())
		transaction := output.transactions["file.bin"]
		expected, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
		if result.Outcome != DirectTreeOutcomePaused || result.TerminationCause != nil ||
			result.SettlementFault != expected || transaction == nil ||
			transaction.commitCalls != 1 || len(transaction.pauseReasons) != 0 ||
			output.completeCalls != 1 || output.pauseCalls != 0 {
			t.Fatalf("result=%+v transaction=%+v", result, transaction)
		}
	})

	t.Run("complete deadline remains typed across the command boundary", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](94)
		output := newJobOutput(share)
		output.finishErr = context.DeadlineExceeded
		job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})

		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || result.TerminationInterruption != 0 ||
			result.SettlementInterruption != TransferInterruptionDeadline {
			t.Fatalf("deadline result=%+v", result)
		}
	})
}

func TestTransferLifecycleTraceCarriesStableTypedMilestones(t *testing.T) {
	share := transferID[catalog.ShareInstance](70)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	var traces []TransferLifecycleTrace
	job, _ := branchJob(t, output, revisions, scriptedRangeReader{})
	job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		traces = append(traces, event)
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	wantStages := []TransferLifecycleStage{
		TransferAdmissionStarted, TransferAdmissionCompleted, TransferDiscoveryStarted,
		TransferGenerationCommitted, TransferDirectoryAdmitted, TransferDirectoryFinalized,
		TransferFileEnqueued, TransferFileStarted, TransferFileAdmitted, TransferFileFirstWrite,
		TransferFileSettled, TransferDiscoveryCompleted, TransferJobSettled,
	}
	if len(traces) != len(wantStages) {
		t.Fatalf("traces=%+v", traces)
	}
	indices := make(map[TransferLifecycleStage]int, len(traces))
	for index, trace := range traces {
		indices[trace.Stage] = index
		if trace.TransferJobID != result.TransferJobID || trace.ReceiveIntentDigest != result.ReceiveIntentDigest || trace.ReceiveIntentDigest.IsZero() {
			t.Fatalf("trace[%d] correlation=%+v result=(%x,%x)", index, trace, result.TransferJobID, result.ReceiveIntentDigest)
		}
		if trace.ReceiveOperationID != result.ReceiveIntent.OperationID() ||
			trace.PlanKind != result.ReceiveIntent.MaterializationPlan().Kind() ||
			!trace.ProtocolSessionID.IsZero() {
			t.Fatalf("trace[%d] runtime correlation=%+v", index, trace)
		}
	}
	for _, stage := range wantStages {
		if _, exists := indices[stage]; !exists {
			t.Fatalf("missing stage %d in %+v", stage, traces)
		}
	}
	if !(indices[TransferAdmissionStarted] < indices[TransferAdmissionCompleted] &&
		indices[TransferAdmissionCompleted] < indices[TransferDiscoveryStarted] &&
		indices[TransferDiscoveryStarted] < indices[TransferGenerationCommitted] &&
		indices[TransferGenerationCommitted] < indices[TransferDirectoryAdmitted] &&
		indices[TransferFileEnqueued] < indices[TransferFileStarted] &&
		indices[TransferFileStarted] < indices[TransferFileAdmitted] &&
		indices[TransferFileAdmitted] < indices[TransferFileFirstWrite] &&
		indices[TransferFileFirstWrite] < indices[TransferFileSettled] &&
		indices[TransferFileSettled] < indices[TransferDirectoryFinalized] &&
		indices[TransferDirectoryFinalized] < indices[TransferJobSettled] &&
		indices[TransferDiscoveryStarted] < indices[TransferDiscoveryCompleted] &&
		indices[TransferDiscoveryCompleted] < indices[TransferJobSettled]) {
		t.Fatalf("invalid lifecycle order: indices=%v traces=%+v", indices, traces)
	}
	fileSettlement := traces[indices[TransferFileSettled]]
	discovery := traces[indices[TransferDiscoveryCompleted]]
	settled := traces[indices[TransferJobSettled]]
	if fileSettlement.FileSettlement != FilePublished ||
		discovery.Discovery != DiscoveryComplete || discovery.ConnectionSizeClass != ConnectionSizeSmall ||
		discovery.Progress.DiscoveredFiles != 1 || settled.Progress.PublishedFiles != 1 ||
		settled.DirectTreeSettlement != DirectTreeSettlementSuccess {
		t.Fatalf("typed lifecycle context=%+v", traces)
	}
	for _, stage := range []TransferLifecycleStage{
		TransferFileEnqueued, TransferFileStarted, TransferFileAdmitted, TransferFileFirstWrite,
		TransferFileSettled,
	} {
		fileTrace := traces[indices[stage]]
		if fileTrace.FileSelection != FileSelectionInherited {
			t.Fatalf("file stage %d context=%+v", stage, fileTrace)
		}
	}
}

func TestFileTransferProgressRejectsUnrelatedSettlementCheckpointCoverage(t *testing.T) {
	binding, _ := outputLifecycleFixture(t)
	empty, err := content.NewRangeSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := VerifyDurableRanges(binding, 0, empty)
	if err != nil {
		t.Fatal(err)
	}
	transaction := &jobFileTransaction{binding: binding}
	progress, recovered, valid := newFileTransferProgress(transaction, initial, true)
	if !valid || recovered != 0 || !progress.beginPending(content.Range{Offset: 0, End: 1}) {
		t.Fatalf("initial file progress = %+v, recovered=%d valid=%t", progress, recovered, valid)
	}
	unrelated, err := content.NewRangeSet([]content.Range{{Offset: 1, End: binding.ExactSize()}})
	if err != nil {
		t.Fatal(err)
	}
	future, err := VerifyDurableRanges(binding, 1, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := NewVerifiedFileSettlement(FilePaused, future)
	if err != nil {
		t.Fatal(err)
	}
	if delta, accepted := progress.reconcileSettlement(transaction, settlement); accepted || delta != 0 {
		t.Fatalf("unrelated settlement checkpoint advanced progress: delta=%d progress=%+v", delta, progress)
	}
}

func TestTransferLifecycleFirstWriteRequiresNewDurableContent(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		name := "already durable transaction"
		if immediate {
			name = "immediate settlement"
		}
		t.Run(name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](74)
			output := newJobOutput(share)
			revisions := &jobRevisionClient{}
			job, file := branchJob(t, output, revisions, scriptedRangeReader{})
			descriptor := revisions.opened[file].Descriptor
			full, err := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize()}})
			if err != nil {
				t.Fatal(err)
			}
			if immediate {
				locator, err := NewPathMaterializationLocator("file.bin")
				if err != nil {
					t.Fatal(err)
				}
				target, err := NewFileMaterializationTarget(output.SessionID(), descriptor, locator)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte("file.bin"))
				var identity OwnedObjectID
				copy(identity[:], digest[:])
				binding, err := BindFileMaterializationTarget(target, identity)
				if err != nil {
					t.Fatal(err)
				}
				checkpoint, err := VerifyDurableRanges(binding, 1, full)
				if err != nil {
					t.Fatal(err)
				}
				output.immediate["file.bin"], err = NewVerifiedFileSettlement(FilePublished, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				output.durable["file.bin"] = full
			}
			var stages []TransferLifecycleStage
			job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
				stages = append(stages, event.Stage)
			})
			if result := job.Run(context.Background()); result.Outcome != DirectTreeOutcomeSuccess {
				t.Fatalf("result=%+v", result)
			}
			if !slices.Contains(stages, TransferFileAdmitted) || slices.Contains(stages, TransferFileFirstWrite) {
				t.Fatalf("stages=%v", stages)
			}
		})
	}
}
