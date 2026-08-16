package transfer

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type nilReleaseCatalog struct{ err error }

func (catalogSource nilReleaseCatalog) AcquireDirectory(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	return catalog.DirectorySnapshot{}, nil, catalogSource.err
}

func (nilReleaseCatalog) OpenDirectoryPages(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	return nil, nil
}

type prefixFailureCatalog struct {
	root      catalog.DirectoryID
	branch    catalog.DirectoryID
	rootPages catalog.DirectorySnapshot
	first     catalog.CatalogPage
	failure   error
}

func (source prefixFailureCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	switch directory {
	case source.root:
		return snapshotPages(source.rootPages), nil
	case source.branch:
		return &prefixFailureCursor{first: source.first, failure: source.failure}, nil
	default:
		return nil, sessionProtocolFailure(ErrCatalogIdentity)
	}
}

type prefixFailureCursor struct {
	first   catalog.CatalogPage
	failure error
	step    uint8
}

func (cursor *prefixFailureCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.CatalogPage{}, false, err
	}
	if cursor.step == 0 {
		cursor.step++
		return cursor.first, true, nil
	}
	return catalog.CatalogPage{}, false, cursor.failure
}

func (*prefixFailureCursor) Close() error { return nil }

type rootPrefixFailureCatalog struct {
	directory catalog.DirectoryID
	first     catalog.CatalogPage
	failure   error
}

func (source rootPrefixFailureCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory != source.directory {
		return nil, sessionProtocolFailure(ErrCatalogIdentity)
	}
	return &prefixFailureCursor{first: source.first, failure: source.failure}, nil
}

func TestTransferJobRejectsMissingCatalogLeaseOnFailurePath(t *testing.T) {
	share := transferID[catalog.ShareInstance](146)
	root := transferID[catalog.DirectoryID](147)
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   nilReleaseCatalog{err: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("directory unavailable"))},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != fault.DependencyContractFault() ||
		!isJobTerminalError(result.TerminationCause) {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobRollsBackSelectedPrefixOfFailedGeneration(t *testing.T) {
	share := transferID[catalog.ShareInstance](140)
	root := transferID[catalog.DirectoryID](141)
	branch := transferID[catalog.DirectoryID](142)
	sibling := transferID[catalog.FileID](144)
	// Keep the failed prefix distinct from the surviving sibling. Identity claims
	// are rejected at discovery time, before a failed branch can be isolated.
	partial := transferID[catalog.FileID](143)
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share,
		DirectoryID:   branch,
		Generation:    transferID[catalog.DirectoryGeneration](2),
		Entries:       []catalog.Entry{jobEntry(t, partial, "partial.bin", 0)},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := NewSelectionRules(true, nil)
	descriptor := jobDescriptor(t, share, sibling, 1, 0)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](145), descriptor)
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{sibling: opened},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         rules,
		Catalog: prefixFailureCatalog{
			root: root, branch: branch,
			rootPages: jobSnapshot(
				t, share, root, 1,
				jobDirectoryEntry(t, branch, "branch"),
				jobEntry(t, sibling, "sibling.bin", 0),
			),
			first: first, failure: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("second page unavailable")),
		},
		Revisions:    revisions,
		Blocks:       scriptedRangeReader{},
		Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.SucceededFiles != 1 ||
		len(result.Directories) != 1 || len(result.Files) != 0 ||
		!slices.Equal(revisions.order, []catalog.FileID{sibling}) ||
		!slices.Equal(output.directories, []string{""}) ||
		!slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v revision opens=%v directories=%v finalized=%v", result, revisions.order, output.directories, output.finalized)
	}
}

func TestTransferJobPausesWithoutAdmittingFailedRootPrefix(t *testing.T) {
	share := transferID[catalog.ShareInstance](130)
	root := transferID[catalog.DirectoryID](131)
	file := transferID[catalog.FileID](132)
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share,
		DirectoryID:   root,
		Generation:    transferID[catalog.DirectoryGeneration](2),
		Entries:       []catalog.Entry{jobEntry(t, file, "prefix.bin", 0)},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := NewSelectionRules(true, nil)
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error)}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         rules,
		Catalog: rootPrefixFailureCatalog{
			directory: root, first: first,
			failure: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("root terminal page unavailable")),
		},
		Revisions:    revisions,
		Blocks:       scriptedRangeReader{},
		Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || len(result.Directories) != 1 ||
		!result.SelectionObservation.IsZero() ||
		result.TerminationFault != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) ||
		len(revisions.order) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("result=%+v revision opens=%v", result, revisions.order)
	}
}

func TestTransferJobPausesWhenRootAdmissionFailsEvenIfFilesAreIsolatable(t *testing.T) {
	share := transferID[catalog.ShareInstance](133)
	output := newJobOutput(share)
	output.ensureFailures = map[string]error{"": errors.New("root output authority unavailable")}
	revisions := &jobRevisionClient{}
	job, _ := branchJob(t, output, revisions, scriptedRangeReader{})

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationCause == nil ||
		len(result.Directories) != 0 || len(result.Files) != 0 || result.SucceededFiles != 0 ||
		len(revisions.order) != 0 || len(revisions.released) != 0 ||
		output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("root admission result=%+v revisions=%v released=%v pause=%d complete=%d", result, revisions.order, revisions.released, output.pauseCalls, output.completeCalls)
	}
}

func TestTransferJobAppliesSyntheticRootOverrideToProbeAndExecution(t *testing.T) {
	for _, selected := range []bool{true, false} {
		name := "deselected"
		if selected {
			name = "selected"
		}
		t.Run(name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](170)
			root := transferID[catalog.DirectoryID](171)
			file := transferID[catalog.FileID](172)
			rules, _ := NewSelectionRules(!selected, []SelectionOverride{{DirectoryID: root, Selected: selected}})
			revisions := &jobRevisionClient{
				opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
			}
			if selected {
				descriptor := jobDescriptor(t, share, file, 1, 0)
				revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](173), descriptor)
			}
			output := newJobOutput(share)
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
						root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "root.bin", 0)),
					}, failures: make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}
			updates := job.ProgressSnapshots()
			result := job.Run(context.Background())
			var admission ReceiveProgressSnapshot
			for measure := range updates {
				admission = measure
			}
			var wantFiles uint64
			if selected {
				wantFiles = 1
			}
			if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != wantFiles ||
				result.Progress.DiscoveredFiles != wantFiles || admission.DiscoveredFiles != wantFiles ||
				result.Progress.ConnectionSizeClass() != ConnectionSizeSmall || admission.ConnectionSizeClass() != ConnectionSizeSmall {
				t.Fatalf("outcome=%v succeeded=%d want=%d term=%v settleFailure=%v settlement=%v files=%+v dirs=%+v measure=%+v admission=%+v output=%+v", result.Outcome, result.SucceededFiles, wantFiles, result.TerminationCause, result.SettlementFailure, result.Settlement, result.Files, result.Directories, result.Progress, admission, output)
			}
		})
	}
}

func TestTransferJobPreservesParentCancellationCauseWithoutSelection(t *testing.T) {
	share := transferID[catalog.ShareInstance](181)
	root := transferID[catalog.DirectoryID](182)
	rules, _ := NewSelectionRules(false, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: make(map[catalog.DirectoryID]catalog.DirectorySnapshot), failures: make(map[catalog.DirectoryID]error)},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("caller stopped this job")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	result := job.Run(ctx)
	if result.Outcome != DirectTreeOutcomePaused || !errors.Is(result.TerminationCause, context.Canceled) ||
		result.TerminationFault.Valid() {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobMaterializesOnlyAuthenticatedSelectedOutput(t *testing.T) {
	t.Run("file target materializes only authenticated selected ancestors", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](150)
		root := transferID[catalog.DirectoryID](151)
		wanted := transferID[catalog.DirectoryID](152)
		unrelated := transferID[catalog.DirectoryID](153)
		file := transferID[catalog.FileID](154)
		rules, _ := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
		descriptor := jobDescriptor(t, share, file, 1, 0)
		opened, _ := NewOpenedRevision(transferID[content.LeaseID](155), descriptor)
		output := newJobOutput(share)
		output.ensureFailures = map[string]error{"unrelated": errors.New("unrelated output must remain virtual")}
		job, err := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root:      jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, unrelated, "unrelated"), jobDirectoryEntry(t, wanted, "wanted")),
					wanted:    jobSnapshot(t, share, wanted, 2, jobEntry(t, file, "file.bin", 0)),
					unrelated: jobSnapshot(t, share, unrelated, 3),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{
				opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
			},
			Blocks: scriptedRangeReader{}, Materializer: output,
		})
		if err != nil {
			t.Fatal(err)
		}
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 {
			t.Fatalf("result=%+v", result)
		}
		if !slices.Equal(output.directories, []string{"", "wanted"}) || !slices.Equal(output.finalized, []string{"wanted", ""}) {
			t.Fatalf("directories=%v finalized=%v", output.directories, output.finalized)
		}
	})

	t.Run("revision failure leaves discovery ancestors virtual", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](156)
		root := transferID[catalog.DirectoryID](157)
		folder := transferID[catalog.DirectoryID](158)
		file := transferID[catalog.FileID](159)
		rules, _ := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
		output := newJobOutput(share)
		job, _ := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root:   jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, folder, "folder")),
					folder: jobSnapshot(t, share, folder, 2, jobEntry(t, file, "missing.bin", 0)),
				}, failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{
				opened:   make(map[catalog.FileID]OpenedRevision),
				failures: map[catalog.FileID]error{file: sourceUnavailableFailure(errors.New("revision unavailable"))},
			},
			Blocks: scriptedRangeReader{}, Materializer: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePartial || len(result.Files) != 1 ||
			!slices.Equal(output.directories, []string{"", "folder"}) || !slices.Equal(output.finalized, []string{"folder", ""}) {
			t.Fatalf("result=%+v directories=%v finalized=%v", result, output.directories, output.finalized)
		}
	})
}

func TestTransferJobSelectedDirectoryRequiresSuccessfulGenerationBeforeOutput(t *testing.T) {
	share := transferID[catalog.ShareInstance](160)
	root := transferID[catalog.DirectoryID](161)
	empty := transferID[catalog.DirectoryID](162)
	failed := transferID[catalog.DirectoryID](163)
	rules, _ := NewSelectionRules(false, []SelectionOverride{
		{DirectoryID: empty, Selected: true}, {DirectoryID: failed, Selected: true},
	})
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, empty, "empty"), jobDirectoryEntry(t, failed, "failed")),
				empty: jobSnapshot(t, share, empty, 2),
			},
			failures: map[catalog.DirectoryID]error{failed: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("directory unavailable"))},
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || len(result.Directories) != 1 || result.Progress.ConnectionSizeClass() != ConnectionSizeUnknown {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(output.directories, []string{"", "empty"}) ||
		!slices.Equal(output.finalized, []string{"empty", ""}) {
		t.Fatalf("directories=%v finalized=%v", output.directories, output.finalized)
	}
}

func TestTransferJobMissingOpaqueTargetsRemainKindSafe(t *testing.T) {
	share := transferID[catalog.ShareInstance](164)
	root := transferID[catalog.DirectoryID](165)
	collidingDirectory := transferID[catalog.DirectoryID](166)
	collidingFile := catalog.FileID(collidingDirectory)
	rules, _ := NewSelectionRules(false, []SelectionOverride{{FileID: collidingFile, Selected: true}})
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:               jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, collidingDirectory, "directory")),
				collidingDirectory: jobSnapshot(t, share, collidingDirectory, 2),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		len(result.Directories) != 0 || !errors.Is(result.SelectionResolutionFailure, ErrSelectionTargetMissing) ||
		output.aborted || output.pauseCalls != 0 || output.completeCalls != 1 ||
		!slices.Equal(output.directories, []string{""}) || !slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobMissingOpaqueDirectoryTargetAborts(t *testing.T) {
	share := transferID[catalog.ShareInstance](167)
	root := transferID[catalog.DirectoryID](168)
	missing := transferID[catalog.DirectoryID](169)
	rules, _ := NewSelectionRules(false, []SelectionOverride{{DirectoryID: missing, Selected: true}})
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		len(result.Directories) != 0 || !errors.Is(result.SelectionResolutionFailure, ErrSelectionTargetMissing) ||
		output.aborted || output.pauseCalls != 0 || output.completeCalls != 1 ||
		!slices.Equal(output.directories, []string{""}) || !slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobUnmatchedFileBelowFailedDirectoryRemainsUnknown(t *testing.T) {
	share := transferID[catalog.ShareInstance](174)
	root := transferID[catalog.DirectoryID](175)
	branch := transferID[catalog.DirectoryID](176)
	file := transferID[catalog.FileID](177)
	rules, _ := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, branch, "branch")),
			},
			failures: map[catalog.DirectoryID]error{branch: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("branch unavailable"))},
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	updates := job.ProgressSnapshots()
	result := job.Run(context.Background())
	var admission ReceiveProgressSnapshot
	for measure := range updates {
		admission = measure
	}
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		result.Progress.ConnectionSizeClass() != ConnectionSizeUnknown || admission.ConnectionSizeClass() != ConnectionSizeUnknown ||
		!slices.Equal(output.directories, []string{""}) || !slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v admission=%+v directories=%v finalized=%v", result, admission, output.directories, output.finalized)
	}
}

func TestTransferJobMissingPathDescendantLeavesAncestorVirtual(t *testing.T) {
	share := transferID[catalog.ShareInstance](178)
	root := transferID[catalog.DirectoryID](179)
	folder := transferID[catalog.DirectoryID](180)
	rules, _ := NewPathSelectionRules([]string{"folder/missing.bin"})
	output := newJobOutput(share)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:   jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, folder, "folder")),
				folder: jobSnapshot(t, share, folder, 2),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		len(result.Directories) != 0 || !errors.Is(result.SelectionResolutionFailure, ErrSelectionTargetMissing) ||
		output.aborted || output.pauseCalls != 0 || output.completeCalls != 1 ||
		!slices.Equal(output.directories, []string{""}) || !slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v directories=%v finalized=%v", result, output.directories, output.finalized)
	}
}
