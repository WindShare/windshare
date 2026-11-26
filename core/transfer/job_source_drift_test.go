package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestTransferJobOnlySourceChangedFilesMatchesRuntimeOutcomes(t *testing.T) {
	const chunk = uint64(catalog.MinChunkSize)
	runScenario := func(t *testing.T, extraFailure error, reader RangeReader) JobResult {
		t.Helper()
		share := transferID[catalog.ShareInstance](0x91)
		root := transferID[catalog.DirectoryID](0x92)
		good := transferID[catalog.FileID](0x93)
		drifted := transferID[catalog.FileID](0x94)
		extra := transferID[catalog.FileID](0x95)
		entries := []catalog.Entry{
			jobEntry(t, good, "good.bin", chunk),
			jobEntry(t, drifted, "drifted.bin", chunk),
		}
		if extraFailure != nil {
			entries = append(entries, jobEntry(t, extra, "extra.bin", chunk))
		}
		goodDesc := jobDescriptor(t, share, good, 0x96, chunk)
		goodOpen, err := NewOpenedRevision(transferID[RevisionHandle](0x97), goodDesc)
		if err != nil {
			t.Fatal(err)
		}
		revisions := &jobRevisionClient{
			opened:   map[catalog.FileID]OpenedRevision{good: goodOpen},
			failures: map[catalog.FileID]error{drifted: sourceChangedFailure(errors.New("stale"))},
		}
		if extraFailure != nil {
			revisions.failures[extra] = extraFailure
		}
		if reader == nil {
			reader = scriptedRangeReader{}
		}
		rules, err := NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		job, err := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root: jobSnapshot(t, share, root, 0x98, entries...),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: revisions, Blocks: reader, Materializer: newJobOutput(share),
		})
		if err != nil {
			t.Fatal(err)
		}
		return job.Run(context.Background())
	}

	pureOpenDrift := runScenario(t, nil, nil)
	if !pureOpenDrift.OnlySourceChangedFiles() {
		t.Fatalf("pure open-time source drift returned false: %+v", pureOpenDrift)
	}

	mixedSiblingFailure := runScenario(t, sourceUnavailableFailure(errors.New("busy")), nil)
	if mixedSiblingFailure.OnlySourceChangedFiles() {
		t.Fatalf("mixed drift with transient sibling failure returned true: %+v", mixedSiblingFailure)
	}

	// Mid-transfer revision invalidation retires only the changed file and still
	// satisfies OnlySourceChangedFiles when all other discovered files publish.
	share := transferID[catalog.ShareInstance](0xa1)
	root := transferID[catalog.DirectoryID](0xa2)
	good := transferID[catalog.FileID](0xa3)
	invalidated := transferID[catalog.FileID](0xa4)
	goodOpen, err := NewOpenedRevision(transferID[RevisionHandle](0xa5), jobDescriptor(t, share, good, 0xa6, chunk))
	if err != nil {
		t.Fatal(err)
	}
	invDesc := jobDescriptor(t, share, invalidated, 0xa7, chunk)
	invOpen, err := NewOpenedRevision(transferID[RevisionHandle](0xa8), invDesc)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	invJob, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 0xa9,
					jobEntry(t, good, "good.bin", chunk),
					jobEntry(t, invalidated, "inv.bin", chunk),
				),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened:   map[catalog.FileID]OpenedRevision{good: goodOpen, invalidated: invOpen},
			failures: make(map[catalog.FileID]error),
		},
		Blocks: rangeReaderFunc(func(ctx context.Context, handle RevisionHandle, desc content.FileRevisionDescriptor, requested content.Range, sink RangeSink) error {
			if desc.FileID() == invalidated {
				return sourceInvalidatedFailure(ErrBlockInvalidated)
			}
			return (scriptedRangeReader{}).ReadRange(ctx, handle, desc, requested, sink)
		}),
		Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	invResult := invJob.Run(context.Background())
	if !invResult.OnlySourceChangedFiles() {
		t.Fatalf("pure mid-transfer invalidation returned false: %+v", invResult)
	}
}

func TestOnlySourceChangedFilesRequiresEveryInvariant(t *testing.T) {
	drift := mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged)
	stale := mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale)
	session := mustSessionFault(fault.ScopeSessionTerminal, fault.SessionTransport)
	output := mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO)

	base := func() JobResult {
		return JobResult{
			Outcome: DirectTreeOutcomePartial, SourceDriftFault: drift,
			Files: []FileJobFailure{{Path: "d/b.txt", Stage: FailureRevisionOpen, Fault: drift}},
			Progress: ReceiveProgressSnapshot{
				Discovery: DiscoveryComplete, CountersExact: true,
				DiscoveredFiles: 2, PublishedFiles: 1,
				FileOutcomes: FileOutcomeSummary{DownloadedFiles: 1},
			},
		}
	}
	listed := func(r *JobResult, failure FileJobFailure) {
		r.Files = append(r.Files, failure)
		r.Progress.DiscoveredFiles++
	}
	for _, test := range []struct {
		name   string
		mutate func(*JobResult)
		want   bool
	}{
		{"only changed file", func(*JobResult) {}, true},
		{"paused outcome", func(r *JobResult) { r.Outcome = DirectTreeOutcomePaused }, false},
		{"failed outcome", func(r *JobResult) { r.Outcome = DirectTreeOutcomeFailed }, false},
		{"no failed files", func(r *JobResult) {
			r.Files = nil
			r.Progress.DiscoveredFiles = 1
		}, false},
		{"directory failure present", func(r *JobResult) {
			r.Directories = []DirectoryJobFailure{{Path: "d/sub", Fault: stale}}
		}, false},
		{"missing selection", func(r *JobResult) { r.SelectionResolutionFailure = ErrSelectionTargetMissing }, false},
		{"no drift fault", func(r *JobResult) { r.SourceDriftFault = fault.Fault{} }, false},
		{"termination interruption", func(r *JobResult) {
			r.TerminationCause, r.TerminationInterruption = context.Canceled, TransferInterruptionCanceled
		}, false},
		{"incomplete discovery", func(r *JobResult) { r.Progress.Discovery = DiscoveryOpen }, false},
		{"failed discovery", func(r *JobResult) { r.Progress.Discovery = DiscoveryFailed }, false},
		{"inexact counters", func(r *JobResult) { r.Progress.CountersExact = false }, false},
		{"omitted file failure", func(r *JobResult) { r.OmittedFileFailures = 1 }, false},
		{"omitted directory failure", func(r *JobResult) { r.OmittedDirectoryFailures = 1 }, false},
		{"tree settlement failure", func(r *JobResult) { r.SettlementFailure = errors.New("finalize") }, false},
		{"tree settlement fault", func(r *JobResult) { r.SettlementFault = output }, false},
		{"collision file present", func(r *JobResult) { r.Progress.FileOutcomes.CollisionFiles = 1 }, false},
		{"item blocked file present", func(r *JobResult) { r.Progress.FileOutcomes.ItemBlockedFiles = 1 }, false},
		{"published count mismatch", func(r *JobResult) { r.Progress.FileOutcomes.DownloadedFiles = 0 }, false},
		{"sibling session failure", func(r *JobResult) { listed(r, FileJobFailure{Path: "d/c.bin", Fault: session}) }, false},
		{"sibling output failure", func(r *JobResult) { listed(r, FileJobFailure{Path: "d/c.bin", Fault: output}) }, false},
		{"changed file with settlement fault", func(r *JobResult) { r.Files[0].SettlementFault = output }, false},
		{"changed file with file interruption", func(r *JobResult) { r.Files[0].Interruption = TransferInterruptionCanceled }, false},
		{"unchanged file unfinished", func(r *JobResult) { r.Progress.DiscoveredFiles++ }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := base()
			test.mutate(&result)
			if got := result.OnlySourceChangedFiles(); got != test.want {
				t.Fatalf("OnlySourceChangedFiles() = %t, want %t", got, test.want)
			}
		})
	}
}

type rangeReaderFunc func(context.Context, RevisionHandle, content.FileRevisionDescriptor, content.Range, RangeSink) error

func (fn rangeReaderFunc) ReadRange(
	ctx context.Context, handle RevisionHandle, desc content.FileRevisionDescriptor, requested content.Range, sink RangeSink,
) error {
	return fn(ctx, handle, desc, requested, sink)
}
