package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestJobRunConstructionRejectsMissingSpoolAuthorities(t *testing.T) {
	if _, err := newJobRun(&TransferJob{}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("zero-share job run = %v", err)
	}
	if _, err := newJobRun(&TransferJob{
		share: transferID[catalog.ShareInstance](0xa1),
	}); !errors.Is(err, ErrSelectionPlanState) {
		t.Fatalf("zero-root job run = %v", err)
	}
}

func TestTerminalDiscoveryFailsClosedWhenSpoolStateChangesAtPageBoundary(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xa2)
	root := transferID[catalog.DirectoryID](0xa3)
	fileID := transferID[catalog.FileID](0xa4)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootPage := jobSnapshot(t, share, root, 1, jobEntry(t, fileID, "file.bin", 0)).Pages()[0]

	t.Run("checkpoint after close", func(t *testing.T) {
		run := newTraversalBoundaryRun(t, share, root, rules, nil)
		if err := run.spool.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := run.discoverDirectory(context.Background(), root, "", catalog.ModifiedTime{}, true); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("closed-spool discovery = %v", err)
		}
	})

	t.Run("empty cursor", func(t *testing.T) {
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{}, nil
		})
		run := newTraversalBoundaryRun(t, share, root, rules, source)
		if _, err := run.discoverDirectory(context.Background(), root, "", catalog.ModifiedTime{}, true); !errors.Is(err, ErrCatalogIdentity) {
			t.Fatalf("empty generation = %v", err)
		}
	})

	t.Run("context cancellation before entry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{page: rootPage, beforeReturn: cancel}, nil
		})
		run := newTraversalBoundaryRun(t, share, root, rules, source)
		if _, err := run.discoverDirectory(ctx, root, "", catalog.ModifiedTime{}, true); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled entry discovery = %v", err)
		}
	})

	t.Run("directory append budget", func(t *testing.T) {
		child := transferID[catalog.DirectoryID](0xa5)
		childPage := jobSnapshot(t, share, child, 2).Pages()[0]
		var run *jobRun
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{
				page: childPage,
				beforeReturn: func() {
					run.spool.rawBytes = maximumSelectionPlanBytes
				},
			}, nil
		})
		run = newTraversalBoundaryRun(t, share, root, rules, source)
		if _, err := run.discoverDirectory(context.Background(), child, "child", catalog.ModifiedTime{}, true); !errors.Is(err, ErrSelectionPlanBudget) {
			t.Fatalf("directory append budget = %v", err)
		}
	})

	t.Run("identity claim after freeze", func(t *testing.T) {
		var run *jobRun
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{
				page: rootPage,
				beforeReturn: func() {
					if err := run.spool.Freeze(context.Background()); err != nil {
						t.Errorf("freeze boundary spool: %v", err)
					}
				},
			}, nil
		})
		run = newTraversalBoundaryRun(t, share, root, rules, source)
		if _, err := run.discoverDirectory(context.Background(), root, "", catalog.ModifiedTime{}, true); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("claim after freeze = %v", err)
		}
	})

	t.Run("file append budget", func(t *testing.T) {
		var run *jobRun
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{
				page: rootPage,
				beforeReturn: func() {
					run.spool.rawBytes = maximumSelectionPlanBytes
				},
			}, nil
		})
		run = newTraversalBoundaryRun(t, share, root, rules, source)
		if _, err := run.discoverDirectory(context.Background(), root, "", catalog.ModifiedTime{}, true); !errors.Is(err, ErrSelectionPlanBudget) {
			t.Fatalf("file append budget = %v", err)
		}
	})
}

func TestTerminalDiscoverySeparatesFreezeAndCanonicalizationFailures(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xa6)
	root := transferID[catalog.DirectoryID](0xa7)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootPage := jobSnapshot(t, share, root, 3).Pages()[0]

	t.Run("freeze", func(t *testing.T) {
		var run *jobRun
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{
				page: rootPage,
				closeHook: func() error {
					return run.spool.Freeze(context.Background())
				},
			}, nil
		})
		run = newTraversalBoundaryRun(t, share, root, rules, source)
		if _, _, err := run.discoverSelection(context.Background()); !errors.Is(err, ErrSelectionPlanState) {
			t.Fatalf("second terminal freeze = %v", err)
		}
	})

	t.Run("canonical request", func(t *testing.T) {
		source := traversalCatalogFunc(func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
			return &traversalBoundaryCursor{page: rootPage}, nil
		})
		run := newTraversalBoundaryRun(t, share, root, rules, source)
		run.job.selectionRequest = CanonicalSelectionRequest{}
		if _, _, err := run.discoverSelection(context.Background()); !errors.Is(err, ErrInvalidOutputSelection) {
			t.Fatalf("unbound canonical request = %v", err)
		}
	})
}

func TestDiscoveryHelpersRejectInvalidEntriesAndNonIsolatableFailures(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xa8)
	root := transferID[catalog.DirectoryID](0xa9)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := newTraversalBoundaryRun(t, share, root, rules, nil)
	if _, err := run.discoverEntry(
		context.Background(), catalog.Entry{}, root,
		transferID[catalog.DirectoryGeneration](0xaa), "", true,
	); !errors.Is(err, ErrCatalogIdentity) {
		t.Fatalf("invalid catalog entry = %v", err)
	}
	terminal := NewJobDependencyContractError(errors.New("dependency failed"))
	if err := run.recordDiscoveryFailure(root, "", terminal); !errors.Is(err, terminal) {
		t.Fatalf("terminal discovery failure = %v", err)
	}
	checkpoint, err := run.spool.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.isolateDiscoveryFailure(checkpoint, root, "", terminal); !errors.Is(err, terminal) {
		t.Fatalf("non-isolatable discovery failure = %v", err)
	}
}

type traversalCatalogFunc func(context.Context, catalog.DirectoryID) (catalog.DirectoryPageCursor, error)

func (function traversalCatalogFunc) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	return function(ctx, directory)
}

type traversalBoundaryCursor struct {
	page         catalog.CatalogPage
	beforeReturn func()
	closeHook    func() error
	delivered    bool
}

func (cursor *traversalBoundaryCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	if cursor.delivered || cursor.page.ShareInstance().IsZero() {
		return catalog.CatalogPage{}, false, nil
	}
	cursor.delivered = true
	if cursor.beforeReturn != nil {
		cursor.beforeReturn()
	}
	return cursor.page, true, nil
}

func (cursor *traversalBoundaryCursor) Close() error {
	if cursor.closeHook != nil {
		return cursor.closeHook()
	}
	return nil
}

func newTraversalBoundaryRun(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	source CatalogReader,
) *jobRun {
	t.Helper()
	request, err := NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	job := &TransferJob{
		share: share, root: root, rules: rules, selectionRequest: request,
		catalog: source, tracker: newSelectionTracker(),
	}
	run, err := newJobRun(job)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.close)
	return run
}
