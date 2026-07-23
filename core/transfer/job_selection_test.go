package transfer

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type nilReleaseCatalog struct{ err error }

func (catalogSource nilReleaseCatalog) AcquireDirectory(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	return catalog.DirectorySnapshot{}, nil, catalogSource.err
}

func TestTransferJobRejectsMissingCatalogLeaseOnFailurePath(t *testing.T) {
	share := transferID[catalog.ShareInstance](146)
	root := transferID[catalog.DirectoryID](147)
	rules, _ := NewSelectionRules(true, nil)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   nilReleaseCatalog{err: jobDirectoryFailure{errors.New("directory unavailable")}},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobAborted || !errors.Is(result.AbortCause, ErrCatalogLeaseContract) ||
		!isJobTerminalError(result.AbortCause) || isSessionFailure(result.AbortCause) {
		t.Fatalf("result=%+v", result)
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
			job, err := NewTransferJob(TransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
						root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "root.bin", 0)),
					}, failures: make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
			})
			if err != nil {
				t.Fatal(err)
			}
			updates := job.SelectionMeasures()
			result := job.Run(context.Background())
			var admission SelectionMeasure
			for measure := range updates {
				admission = measure
			}
			var wantFiles uint64
			if selected {
				wantFiles = 1
			}
			if result.Outcome != JobSucceeded || result.SucceededFiles != wantFiles ||
				result.Measure.DiscoveredFiles != wantFiles || admission.DiscoveredFiles != wantFiles ||
				result.Measure.Class() != SelectionSmall || admission.Class() != SelectionSmall {
				t.Fatalf("result=%+v admission=%+v", result, admission)
			}
		})
	}
}

func TestTransferJobPreservesParentCancellationCauseWithoutSelection(t *testing.T) {
	share := transferID[catalog.ShareInstance](181)
	root := transferID[catalog.DirectoryID](182)
	rules, _ := NewSelectionRules(false, nil)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: make(map[catalog.DirectoryID]catalog.DirectorySnapshot), failures: make(map[catalog.DirectoryID]error)},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("caller stopped this job")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	result := job.Run(ctx)
	if result.Outcome != JobAborted || !errors.Is(result.AbortCause, cause) {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobMaterializesOnlyAuthenticatedSelectedOutput(t *testing.T) {
	t.Run("file target materializes ancestors only after revision opens", func(t *testing.T) {
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
		job, err := NewTransferJob(TransferJobConfig{
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
			Blocks: scriptedRangeReader{}, Output: output,
		})
		if err != nil {
			t.Fatal(err)
		}
		result := job.Run(context.Background())
		if result.Outcome != JobSucceeded || result.SucceededFiles != 1 {
			t.Fatalf("result=%+v", result)
		}
		if !slices.Equal(output.directories, []string{"wanted"}) || !slices.Equal(output.finalized, []string{"wanted"}) {
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
		job, _ := NewTransferJob(TransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root:   jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, folder, "folder")),
					folder: jobSnapshot(t, share, folder, 2, jobEntry(t, file, "missing.bin", 0)),
				}, failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{
				opened: make(map[catalog.FileID]OpenedRevision), failures: map[catalog.FileID]error{file: errors.New("revision unavailable")},
			},
			Blocks: scriptedRangeReader{}, Output: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != JobCompletedWithErrors || len(result.Files) != 1 || len(output.directories) != 0 || len(output.finalized) != 0 {
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
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, empty, "empty"), jobDirectoryEntry(t, failed, "failed")),
				empty: jobSnapshot(t, share, empty, 2),
			},
			failures: map[catalog.DirectoryID]error{failed: jobDirectoryFailure{errors.New("directory unavailable")}},
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobCompletedWithErrors || len(result.Directories) != 1 || result.Measure.Class() != SelectionUnknown {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(output.directories, []string{"empty"}) || !slices.Equal(output.finalized, []string{"empty"}) {
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
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:               jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, collidingDirectory, "directory")),
				collidingDirectory: jobSnapshot(t, share, collidingDirectory, 2),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobAborted || !errors.Is(result.AbortCause, ErrSelectionTargetMissing) || !output.aborted {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobMissingOpaqueDirectoryTargetAborts(t *testing.T) {
	share := transferID[catalog.ShareInstance](167)
	root := transferID[catalog.DirectoryID](168)
	missing := transferID[catalog.DirectoryID](169)
	rules, _ := NewSelectionRules(false, []SelectionOverride{{DirectoryID: missing, Selected: true}})
	output := newJobOutput(share)
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobAborted || !errors.Is(result.AbortCause, ErrSelectionTargetMissing) || !output.aborted {
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
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, branch, "branch")),
			},
			failures: map[catalog.DirectoryID]error{branch: jobDirectoryFailure{errors.New("branch unavailable")}},
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	updates := job.SelectionMeasures()
	result := job.Run(context.Background())
	var admission SelectionMeasure
	for measure := range updates {
		admission = measure
	}
	if result.Outcome != JobCompletedWithErrors || result.AbortCause != nil ||
		result.Measure.Class() != SelectionUnknown || admission.Class() != SelectionUnknown ||
		len(output.directories) != 0 || len(output.finalized) != 0 {
		t.Fatalf("result=%+v admission=%+v directories=%v finalized=%v", result, admission, output.directories, output.finalized)
	}
}

func TestTransferJobMissingPathDescendantLeavesAncestorVirtual(t *testing.T) {
	share := transferID[catalog.ShareInstance](178)
	root := transferID[catalog.DirectoryID](179)
	folder := transferID[catalog.DirectoryID](180)
	rules, _ := NewPathSelectionRules([]string{"folder/missing.bin"})
	output := newJobOutput(share)
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:   jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, folder, "folder")),
				folder: jobSnapshot(t, share, folder, 2),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobAborted || !errors.Is(result.AbortCause, ErrSelectionTargetMissing) ||
		len(output.directories) != 0 || len(output.finalized) != 0 {
		t.Fatalf("result=%+v directories=%v finalized=%v", result, output.directories, output.finalized)
	}
}
