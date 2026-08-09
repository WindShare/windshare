package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestTransferJobSessionFailureAbortsJob(t *testing.T) {
	share := transferID[catalog.ShareInstance](90)
	root := transferID[catalog.DirectoryID](91)
	file := transferID[catalog.FileID](92)
	laterFile := transferID[catalog.FileID](96)
	chunk := uint64(catalog.MinChunkSize)
	snapshot := multiPageJobSnapshot(t, share, root, 93,
		jobEntry(t, file, "file.bin", chunk), jobEntry(t, laterFile, "later.bin", chunk),
	)
	descriptor := jobDescriptor(t, share, file, 94, chunk)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](95), descriptor)
	revisions := &jobRevisionClient{opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error)}
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	terminal := sessionProtocolFailure(errors.New("authenticated terminal"))
	replayBlocked := make(chan struct{})
	catalogSource := &sessionFailureReplayBarrierCatalog{snapshot: snapshot, replayBlocked: replayBlocked}
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: catalogSource, Revisions: revisions,
		Blocks: replayBarrierSessionFailingBlocks{replayBlocked: replayBlocked, err: terminal}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeResumable || result.TerminationFault != normalizedFault(terminal) || !output.aborted || output.finished != 0 ||
		result.Measure.ConnectionSizeClass() != ConnectionSizeSmall || !result.Measure.DiscoveryTerminalSuccess ||
		result.Measure.DiscoveredFiles != 2 || result.Measure.DiscoveredBytes != 2*chunk {
		t.Fatalf("result=%+v output=%+v", result, output)
	}
	transaction := output.transactions["file.bin"]
	if transaction == nil || len(result.Files) != 1 || result.Files[0].Stage != FailureBlockTransfer ||
		result.Files[0].Fault != normalizedFault(terminal) || result.Files[0].Settlement.Kind() != FilePaused ||
		result.Files[0].SettlementFailure != nil || result.Files[0].SettlementFault.Valid() ||
		!transaction.aborted || !slices.Equal(transaction.pauseReasons, []FilePauseReason{FilePauseSessionFailure}) ||
		transaction.commitCalls != 0 || output.transactions["later.bin"] != nil {
		t.Fatalf("result=%+v transaction=%+v output=%+v", result, transaction, output)
	}
	if second := job.Run(context.Background()); second.Outcome != DirectTreeOutcomeResumable || second.TerminationFault != fault.DependencyContractFault() {
		t.Fatalf("second run=%+v", second)
	}
}

type sessionFailureReplayBarrierCatalog struct {
	snapshot      catalog.DirectorySnapshot
	replayBlocked chan struct{}
	blockOnce     sync.Once
	mu            sync.Mutex
	opens         int
}

func (source *sessionFailureReplayBarrierCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory != source.snapshot.DirectoryID() {
		return nil, ErrCatalogIdentity
	}
	source.mu.Lock()
	openIndex := source.opens
	source.opens++
	source.mu.Unlock()
	return &sessionFailureReplayBarrierCursor{
		inner: snapshotPages(source.snapshot), blockBeforeSecondPage: openIndex == 1,
		replayBlocked: source.replayBlocked, blockOnce: &source.blockOnce,
	}, nil
}

type sessionFailureReplayBarrierCursor struct {
	inner                 catalog.DirectoryPageCursor
	blockBeforeSecondPage bool
	nextCalls             int
	replayBlocked         chan struct{}
	blockOnce             *sync.Once
}

func (cursor *sessionFailureReplayBarrierCursor) Next(
	ctx context.Context,
) (catalog.CatalogPage, bool, error) {
	if cursor.blockBeforeSecondPage && cursor.nextCalls == 1 {
		cursor.blockOnce.Do(func() { close(cursor.replayBlocked) })
		<-ctx.Done()
		return catalog.CatalogPage{}, false, ctx.Err()
	}
	cursor.nextCalls++
	return cursor.inner.Next(ctx)
}

func (cursor *sessionFailureReplayBarrierCursor) Close() error { return cursor.inner.Close() }

type replayBarrierSessionFailingBlocks struct {
	replayBlocked <-chan struct{}
	err           error
}

func (reader replayBarrierSessionFailingBlocks) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	_ content.Range,
	_ RangeSink,
) error {
	select {
	case <-reader.replayBlocked:
		return reader.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type retiredCountingRangeReader struct{ calls int }

func (reader *retiredCountingRangeReader) ReadRange(
	context.Context,
	content.LeaseID,
	content.FileRevisionDescriptor,
	content.Range,
	RangeSink,
) error {
	reader.calls++
	return nil
}

func TestTransferJobAcceptsRecoveredImmediateRetirementWithoutContentOrSecondFileSettlement(t *testing.T) {
	share := transferID[catalog.ShareInstance](96)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	blocks := &retiredCountingRangeReader{}
	job, file := branchJob(t, output, revisions, blocks)
	opened := revisions.opened[file]
	locator, err := NewPathMaterializationLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewFileMaterializationTarget(output.SessionID(), opened.Descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	var identity OwnedObjectID
	digest := sha256.Sum256([]byte("recovered-retiring-output"))
	copy(identity[:], digest[:])
	binding, err := BindFileMaterializationTarget(target, identity)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := NewRetiredFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	output.immediate["file.bin"] = retired

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartialDirectory || result.Settlement.Kind() != DirectTreeSettlementPartialDirectory ||
		result.TerminationCause != nil || result.SettlementFailure != nil || result.SucceededFiles != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Files) != 1 || !errors.Is(result.Files[0].Cause, ErrOutputRetired) ||
		result.Files[0].SettlementFailure != nil {
		t.Fatalf("retired file result = %+v", result.Files)
	}
	settledBinding, bound := result.Files[0].Settlement.MaterializedBinding()
	if result.Files[0].Settlement.Kind() != FileRetired || !bound || settledBinding != binding {
		t.Fatalf("retired settlement = %+v", result.Files[0].Settlement)
	}
	if blocks.calls != 0 || len(output.transactions) != 0 || output.pauseCalls != 0 ||
		output.completeCalls != 1 || output.finished != DirectTreeOutcomePartialDirectory {
		t.Fatalf(
			"range calls=%d transactions=%d pause=%d complete=%d outcome=%v",
			blocks.calls, len(output.transactions), output.pauseCalls, output.completeCalls, output.finished,
		)
	}
	if !slices.Equal(revisions.released, []content.LeaseID{opened.LeaseID}) {
		t.Fatalf("released leases = %v, want %v", revisions.released, opened.LeaseID)
	}
}

func TestTransferJobRetiresOnlyWithTypedPermanentSourceAuthority(t *testing.T) {
	tests := []struct {
		name        string
		blocks      RangeReader
		configure   func(*jobOutput)
		wantOutcome DirectTreeOutcome
		wantPause   FilePauseReason
		wantRetire  FileRetireReason
	}{
		{
			name:        "unknown range failure pauses as dependency contract",
			blocks:      scriptedRangeReader{err: errors.New("retryable transport failure")},
			wantOutcome: DirectTreeOutcomeResumable, wantPause: FilePauseDependencyContract,
		},
		{
			name:   "output sink failure cannot impersonate a permanent source failure",
			blocks: scriptedRangeReader{},
			configure: func(output *jobOutput) {
				output.transactionScript.writeErr = outputFailure(
					fault.ScopeFileLocal, fault.OutputStateIO, errors.New("output temporarily unavailable"),
				)
			},
			wantOutcome: DirectTreeOutcomePartialDirectory, wantPause: FilePauseOutputFailure,
		},
		{
			name: "closed output fault cannot acquire source retirement authority",
			blocks: scriptedRangeReader{err: outputFailure(
				fault.ScopeFileLocal, fault.OutputStateIO, errors.New("checkpoint unavailable"),
			)},
			wantOutcome: DirectTreeOutcomePartialDirectory, wantPause: FilePauseOutputFailure,
		},
		{
			name:        "typed permanent source failure retires",
			blocks:      scriptedRangeReader{err: sourcePermanentFailure(errors.New("source denied file"))},
			wantOutcome: DirectTreeOutcomePartialDirectory,
			wantRetire:  FileRetireIsolatedPermanentSourceFailure,
		},
		{
			name:        "invalidated revision retires with its precise reason",
			blocks:      scriptedRangeReader{err: content.ErrRevisionDrift},
			wantOutcome: DirectTreeOutcomePartialDirectory,
			wantRetire:  FileRetireInvalidatedRevision,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](97)
			output := newJobOutput(share)
			if test.configure != nil {
				test.configure(output)
			}
			job, _ := branchJob(t, output, &jobRevisionClient{}, test.blocks)
			result := job.Run(context.Background())
			transaction := output.transactions["file.bin"]
			if transaction == nil || result.Outcome != test.wantOutcome || len(result.Files) != 1 {
				t.Fatalf("result=%+v transaction=%+v", result, transaction)
			}
			if test.wantPause != 0 {
				if !slices.Equal(transaction.pauseReasons, []FilePauseReason{test.wantPause}) ||
					len(transaction.retireReasons) != 0 || result.Files[0].Settlement.Kind() != FilePaused {
					t.Fatalf("pause=%v retire=%v failure=%+v", transaction.pauseReasons, transaction.retireReasons, result.Files[0])
				}
				return
			}
			if !slices.Equal(transaction.retireReasons, []FileRetireReason{test.wantRetire}) ||
				len(transaction.pauseReasons) != 0 || result.Files[0].Settlement.Kind() != FileRetired {
				t.Fatalf("pause=%v retire=%v failure=%+v", transaction.pauseReasons, transaction.retireReasons, result.Files[0])
			}
		})
	}
}

func TestTransferJobContinuesSiblingAfterSettledFileOutputFault(t *testing.T) {
	share := transferID[catalog.ShareInstance](245)
	root := transferID[catalog.DirectoryID](246)
	failedFile := transferID[catalog.FileID](247)
	siblingFile := transferID[catalog.FileID](248)
	entries := []catalog.Entry{
		jobEntry(t, failedFile, "a-failed.bin", 1),
		jobEntry(t, siblingFile, "b-sibling.bin", 1),
	}
	revisions := &jobRevisionClient{
		opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
	}
	for index, file := range []catalog.FileID{failedFile, siblingFile} {
		descriptor := jobDescriptor(t, share, file, byte(249+index), 1)
		opened, openErr := NewOpenedRevision(
			transferID[content.LeaseID](byte(251+index)), descriptor,
		)
		if openErr != nil {
			t.Fatal(openErr)
		}
		revisions.opened[file] = opened
	}
	output := newJobOutput(share)
	writeCause := errors.New("file checkpoint device unavailable")
	output.transactionScripts["a-failed.bin"] = jobTransactionScript{
		writeErr: outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, writeCause),
	}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, entries...),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	failedTransaction := output.transactions["a-failed.bin"]
	siblingTransaction := output.transactions["b-sibling.bin"]
	if result.Outcome != DirectTreeOutcomePartialDirectory || result.TerminationCause != nil ||
		result.SucceededFiles != 1 || len(result.Files) != 1 ||
		result.Files[0].Fault != mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO) ||
		failedTransaction == nil || siblingTransaction == nil ||
		!slices.Equal(failedTransaction.pauseReasons, []FilePauseReason{FilePauseOutputFailure}) ||
		siblingTransaction.commitCalls != 1 || output.pauseCalls != 0 || output.completeCalls != 1 ||
		!slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v failed=%+v sibling=%+v output=%+v", result, failedTransaction, siblingTransaction, output)
	}
}

func TestTransferJobRevisionSessionFailureStopsBeforeNextFile(t *testing.T) {
	const fileCount = 128
	share := transferID[catalog.ShareInstance](253)
	root := transferID[catalog.DirectoryID](254)
	chunk := uint64(catalog.MinChunkSize)
	terminal := sessionTransportFailure(errors.New("receiver runtime closed"))
	entries := make([]catalog.Entry, 0, fileCount)
	failures := make(map[catalog.FileID]error, fileCount)
	for index := range fileCount {
		file := transferID[catalog.FileID](byte(index + 1))
		entries = append(entries, jobEntry(t, file, fmt.Sprintf("file-%03d.bin", index), chunk))
		failures[file] = terminal
	}
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: failures}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 252, entries...),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeResumable || result.TerminationFault != normalizedFault(terminal) ||
		len(result.Files) != 0 || len(revisions.order) != 1 || !output.aborted || output.finished != 0 {
		t.Fatalf(
			"outcome=%v abort=%v file failures=%d revision attempts=%d output aborted=%v finished=%v",
			result.Outcome, result.TerminationCause, len(result.Files), len(revisions.order), output.aborted, output.finished,
		)
	}
}

func TestTransferJobCancellationDuringDiscoveryIsAborted(t *testing.T) {
	share := transferID[catalog.ShareInstance](100)
	root := transferID[catalog.DirectoryID](101)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{failures: map[catalog.DirectoryID]error{root: context.Canceled}},
		Revisions: &jobRevisionClient{}, Blocks: sessionFailingBlocks{}, Materializer: output,
	})
	result := job.Run(ctx)
	if result.Outcome != DirectTreeOutcomeResumable || !errors.Is(result.TerminationCause, context.Canceled) {
		t.Fatalf("result=%+v", result)
	}
}

func TestClosedFaultScopeDrivesOutputPolicyWithoutExposingDiagnostics(t *testing.T) {
	diagnostic := errors.New("journal")
	fatal := outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, diagnostic)
	nonfatal := outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, errors.New("file"))
	capabilities := newJobOutput(transferID[catalog.ShareInstance](1)).Capabilities()
	if !lifecyclePolicyFor(fatal).outputRequiresJobPause(capabilities) ||
		lifecyclePolicyFor(nonfatal).outputRequiresJobPause(capabilities) {
		t.Fatal("output error scope classification failed")
	}
	if errors.Is(fatal, diagnostic) || normalizedFault(fatal).Scope() != fault.ScopeOutputPause {
		t.Fatal("normalized lifecycle fault exposed diagnostic authority")
	}
}

type scriptedRangeReader struct{ err error }

func (r scriptedRangeReader) ReadRange(ctx context.Context, _ content.LeaseID, _ content.FileRevisionDescriptor, requested content.Range, sink RangeSink) error {
	if r.err != nil {
		return r.err
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func branchJob(t *testing.T, output *jobOutput, revisions *jobRevisionClient, blocks RangeReader) (*TransferJob, catalog.FileID) {
	t.Helper()
	share := output.share
	root := transferID[catalog.DirectoryID](111)
	file := transferID[catalog.FileID](112)
	size := uint64(catalog.MinChunkSize)
	snapshot := jobSnapshot(t, share, root, 113, jobEntry(t, file, "file.bin", size))
	if revisions.opened == nil {
		revisions.opened = make(map[catalog.FileID]OpenedRevision)
	}
	if revisions.failures == nil {
		revisions.failures = make(map[catalog.FileID]error)
	}
	if _, exists := revisions.opened[file]; !exists && revisions.failures[file] == nil {
		descriptor := jobDescriptor(t, share, file, 114, size)
		revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](115), descriptor)
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot}, failures: make(map[catalog.DirectoryID]error)},
		Revisions: revisions, Blocks: blocks, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job, file
}

func TestTransferJobValidationEmptySelectionAndFailureBranches(t *testing.T) {
	if _, err := NewOpenedRevision(content.LeaseID{}, content.FileRevisionDescriptor{}); !errors.Is(err, ErrRevisionIdentity) {
		t.Fatalf("invalid opened revision error=%v", err)
	}
	if _, err := NewTransferJob(TransferJobConfig{}); !errors.Is(err, ErrInvalidTransferJob) {
		t.Fatalf("invalid job error=%v", err)
	}
	share := transferID[catalog.ShareInstance](120)
	root := transferID[catalog.DirectoryID](121)
	emptyRules, _ := NewSelectionRules(false, nil)
	emptyOutput := newJobOutput(share)
	baseConfig := testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: emptyRules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: emptyOutput,
	}
	if _, err := NewTransferJob(baseConfig.production()); !errors.Is(err, ErrInvalidTransferJob) {
		t.Fatalf("zero intent accepted: %v", err)
	}
	baseConfig.ReceiveIntent = testReceiveIntent(t, share, root, emptyRules)
	if _, err := NewTransferJob(baseConfig.production()); !errors.Is(err, ErrInvalidTransferJob) {
		t.Fatalf("zero job ID accepted: %v", err)
	}
	baseConfig.JobID = transferID[TransferJobID](122)
	emptyJob, err := NewTransferJob(baseConfig.production())
	if err != nil {
		t.Fatal(err)
	}
	if result := emptyJob.Run(context.Background()); result.Outcome != DirectTreeOutcomePublished || result.Measure.ConnectionSizeClass() != ConnectionSizeSmall || emptyOutput.finished != DirectTreeOutcomePublished {
		t.Fatalf("empty result=%+v", result)
	}

	tests := []struct {
		name             string
		configure        func(*jobOutput, *jobRevisionClient, catalog.FileID)
		blocks           RangeReader
		wantOutcome      DirectTreeOutcome
		wantFailureStage FailureStage
	}{
		{name: "begin file", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, errors.New("file create"))
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomePartialDirectory, wantFailureStage: FailureFileOutput},
		{name: "fatal begin", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("journal"))
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "canceled begin", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "nil transaction contract", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.nilTransaction = true
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "write", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.writeErr = errors.New("disk full")
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable, wantFailureStage: FailureBlockTransfer},
		{name: "checkpoint", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = errors.New("sync failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable, wantFailureStage: FailureFileOutput},
		{name: "fatal checkpoint cannot be skipped", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("backend lost"))
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "canceled checkpoint", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "checkpoint contract", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.omitCheckpoint = true
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "commit", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.commitErr = errors.New("publish failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable, wantFailureStage: FailureFileOutput},
		{name: "unknown release pauses as dependency contract", configure: func(_ *jobOutput, revisions *jobRevisionClient, _ catalog.FileID) {
			revisions.releaseErr = errors.New("release failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable, wantFailureStage: FailureLeaseRelease},
		{name: "canceled release", configure: func(_ *jobOutput, revisions *jobRevisionClient, _ catalog.FileID) {
			revisions.releaseErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "finish", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.finishErr = errors.New("finalize failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "invalid retire settlement", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.retireSettlement = FilePublished
		}, blocks: scriptedRangeReader{err: sourcePermanentFailure(errors.New("block failed"))}, wantOutcome: DirectTreeOutcomeResumable},
		{name: "unknown retire settlement", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.retireSettlement = FileSettlementKind(99)
		}, blocks: scriptedRangeReader{err: sourcePermanentFailure(errors.New("block failed"))}, wantOutcome: DirectTreeOutcomeResumable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := newJobOutput(share)
			revisions := &jobRevisionClient{}
			job, file := branchJob(t, output, revisions, test.blocks)
			test.configure(output, revisions, file)
			result := job.Run(context.Background())
			if result.Outcome != test.wantOutcome {
				t.Fatalf("outcome=%v result=%+v", result.Outcome, result)
			}
			if test.wantFailureStage != 0 && (len(result.Files) == 0 || result.Files[0].Stage != test.wantFailureStage) {
				t.Fatalf("file failures=%+v", result.Files)
			}
		})
	}
}

func TestTransferJobRevisionIdentitySessionReleaseAndStreamSkipBranches(t *testing.T) {
	share := transferID[catalog.ShareInstance](130)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error)}
	job, selectedFile := branchJob(t, output, revisions, scriptedRangeReader{})
	wrongFile := transferID[catalog.FileID](131)
	wrongDescriptor := jobDescriptor(t, share, wrongFile, 132, uint64(catalog.MinChunkSize))
	revisions.opened[selectedFile], _ = NewOpenedRevision(transferID[content.LeaseID](133), wrongDescriptor)
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartialDirectory || len(result.Files) != 1 || result.Files[0].Stage != FailureRevisionIdentity {
		t.Fatalf("identity result=%+v", result)
	}

	output = newJobOutput(share)
	revisions = &jobRevisionClient{releaseErr: sessionTransportFailure(errors.New("session closed"))}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{})
	if result = job.Run(context.Background()); result.Outcome != DirectTreeOutcomeResumable {
		t.Fatalf("session release result=%+v", result)
	}

	streamCapabilities, _ := NewDirectTreeCapabilities(DirectTreeCapabilities{Durability: DurabilityNone})
	output = newJobOutput(share)
	output.capabilitiesOverride = &streamCapabilities
	revisions = &jobRevisionClient{}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{err: errors.New("file unavailable")})
	if result = job.Run(context.Background()); result.Outcome != DirectTreeOutcomeResumable {
		t.Fatalf("unstarted stream result=%+v", result)
	}
}

func TestTransferJobDirectoryOutputAndCatalogIdentityBranches(t *testing.T) {
	share := transferID[catalog.ShareInstance](140)
	root := transferID[catalog.DirectoryID](141)
	child := transferID[catalog.DirectoryID](142)
	rootSnapshot := jobSnapshot(t, share, root, 143, jobDirectoryEntry(t, child, "child"))
	childSnapshot := jobSnapshot(t, share, child, 144)
	rules, _ := NewSelectionRules(true, nil)
	for _, test := range []struct {
		name        string
		ensureErr   error
		finalizeErr error
		want        DirectTreeOutcome
	}{
		{name: "root directory admission pauses after output open", ensureErr: errors.New("mkdir denied"), want: DirectTreeOutcomeResumable},
		{name: "root finalize pauses", finalizeErr: errors.New("mtime denied"), want: DirectTreeOutcomeResumable},
		{name: "ensure fatal", ensureErr: outputFailure(fault.ScopeOutputPause, fault.OutputStateIO, errors.New("backend lost")), want: DirectTreeOutcomeResumable},
		{name: "ensure canceled", ensureErr: context.Canceled, want: DirectTreeOutcomeResumable},
		{name: "finalize deadline", finalizeErr: context.DeadlineExceeded, want: DirectTreeOutcomeResumable},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := newJobOutput(share)
			output.ensureErr, output.finalizeErr = test.ensureErr, test.finalizeErr
			job, _ := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: rootSnapshot, child: childSnapshot}, failures: make(map[catalog.DirectoryID]error)},
				Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if result := job.Run(context.Background()); result.Outcome != test.want {
				t.Fatalf("case=%s result=%+v outputDirs=%v finalized=%v events=%v pause=%d complete=%d", test.name, result, output.directories, output.finalized, output.events, output.pauseCalls, output.completeCalls)
			}
		})
	}
	foreignSnapshot := jobSnapshot(t, share, child, 145)
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: foreignSnapshot}, failures: make(map[catalog.DirectoryID]error)},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if result := job.Run(context.Background()); result.Outcome != DirectTreeOutcomeResumable || result.TerminationCause == nil {
		t.Fatalf("foreign snapshot result=%+v", result)
	}
}
