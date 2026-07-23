package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type selectionMeasureCatalog struct {
	snapshots map[catalog.DirectoryID]catalog.DirectorySnapshot
	failures  map[catalog.DirectoryID]error
	loads     []catalog.DirectoryID
}

type selectionDirectoryFailure struct{ error }

func (selectionDirectoryFailure) DirectoryFailure() {}

func (c *selectionMeasureCatalog) LoadDirectory(_ context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	c.loads = append(c.loads, directory)
	if err := c.failures[directory]; err != nil {
		return catalog.DirectorySnapshot{}, err
	}
	return c.snapshots[directory], nil
}

func (c *selectionMeasureCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := c.LoadDirectory(ctx, directory)
	return snapshot, func() {}, err
}

func jobSnapshotWithOmissions(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	generation byte,
	omitted uint64,
	entries ...catalog.Entry,
) catalog.DirectorySnapshot {
	t.Helper()
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory,
		Generation: transferID[catalog.DirectoryGeneration](generation),
		Entries:    entries, Terminal: true, OmittedCount: omitted,
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestMeasureSelectionStopsAtLargeBeforeCatalogDescendants(t *testing.T) {
	share := transferID[catalog.ShareInstance](90)
	root := transferID[catalog.DirectoryID](91)
	child := transferID[catalog.DirectoryID](92)
	entries := []catalog.Entry{jobDirectoryEntry(t, child, "later")}
	for index := uint64(1); index <= SmallTransferFileLimit+1; index++ {
		entries = append(entries, jobEntry(t, transferID[catalog.FileID](byte(index)), fmt.Sprintf("f-%02d", index), 0))
	}
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root:  jobSnapshot(t, share, root, 1, entries...),
		child: jobSnapshot(t, share, child, 2),
	}}
	rules, _ := NewSelectionRules(true, nil)

	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if measure.Class() != SelectionLarge || measure.DiscoveredFiles != SmallTransferFileLimit || measure.DiscoveryTerminalSuccess {
		t.Fatalf("measure=%+v class=%v", measure, measure.Class())
	}
	if !slices.Equal(reader.loads, []catalog.DirectoryID{root}) {
		t.Fatalf("loads=%x", reader.loads)
	}
}

func TestMeasureSelectionStopsWithinWideSnapshotAtLargeBoundary(t *testing.T) {
	share := transferID[catalog.ShareInstance](115)
	root := transferID[catalog.DirectoryID](116)
	child := transferID[catalog.DirectoryID](117)
	files := make([]catalog.Entry, 0, SmallTransferFileLimit)
	for index := uint64(1); index <= SmallTransferFileLimit; index++ {
		files = append(files, jobEntry(t, transferID[catalog.FileID](byte(index)), fmt.Sprintf("f-%02d", index), 0))
	}
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root, Generation: transferID[catalog.DirectoryGeneration](1),
		Entries: files,
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root, Generation: transferID[catalog.DirectoryGeneration](1),
		PageIndex: 1, Previous: first.Commitment(), Terminal: true,
		Entries: []catalog.Entry{jobDirectoryEntry(t, child, "later")},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{first, second})
	if err != nil {
		t.Fatal(err)
	}
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root: rootSnapshot,
		child: jobSnapshot(t, share, child, 2,
			jobEntry(t, transferID[catalog.FileID](1), "duplicate-from-root", 1),
		),
	}}
	rules, _ := NewSelectionRules(true, nil)
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil || measure.Class() != SelectionLarge || measure.DiscoveredFiles != SmallTransferFileLimit {
		t.Fatalf("measure=%+v err=%v", measure, err)
	}
	if !slices.Equal(reader.loads, []catalog.DirectoryID{root}) {
		t.Fatalf("loads after absorbing Large=%x", reader.loads)
	}
}

func TestMeasureSelectionTreatsOmittedChildrenAsIncomplete(t *testing.T) {
	share := transferID[catalog.ShareInstance](118)
	root := transferID[catalog.DirectoryID](119)
	file := transferID[catalog.FileID](120)
	rules, _ := NewSelectionRules(true, nil)
	for _, test := range []struct {
		name string
		size uint64
		want SelectionClass
	}{
		{name: "visible below threshold", size: 1, want: SelectionUnknown},
		{name: "visible reaches threshold", size: SmallTransferByteLimit, want: SelectionLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshotWithOmissions(t, share, root, 1, 3, jobEntry(t, file, "visible", test.size)),
			}}
			measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
			})
			if err != nil || measure.Class() != test.want || measure.DiscoveryTerminalSuccess {
				t.Fatalf("measure=%+v class=%v err=%v", measure, measure.Class(), err)
			}
		})
	}
}

func TestMeasureSelectionReturnsUnscopedCatalogErrors(t *testing.T) {
	share := transferID[catalog.ShareInstance](121)
	root := transferID[catalog.DirectoryID](122)
	integrityErr := errors.New("catalog signature rejected")
	reader := &selectionMeasureCatalog{failures: map[catalog.DirectoryID]error{root: integrityErr}}
	rules, _ := NewSelectionRules(true, nil)
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if !errors.Is(err, integrityErr) || measure.Class() != SelectionUnknown {
		t.Fatalf("measure=%+v err=%v", measure, err)
	}
}

func TestMeasureSelectionRejectsAuthenticatedPathBeyondDepthBound(t *testing.T) {
	share := transferID[catalog.ShareInstance](123)
	root := replayDirectoryID(1_000)
	directories := make([]catalog.DirectoryID, catalog.MaxPathDepth+1)
	for index := range directories {
		directories[index] = replayDirectoryID(1_001 + index)
	}
	snapshots := make(map[catalog.DirectoryID]catalog.DirectorySnapshot, len(directories)+1)
	current := root
	for index, child := range directories {
		snapshots[current] = jobSnapshot(t, share, current, 1,
			jobDirectoryEntry(t, child, fmt.Sprintf("d-%03d", index)),
		)
		current = child
	}
	snapshots[current] = jobSnapshot(t, share, current, 1)
	reader := &selectionMeasureCatalog{snapshots: snapshots}
	rules, _ := NewSelectionRules(true, nil)
	_, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if !errors.Is(err, ErrCatalogIdentity) || !isSessionFailure(err) {
		t.Fatalf("deep path error=%v", err)
	}
	if len(reader.loads) != catalog.MaxPathDepth+1 {
		t.Fatalf("loaded %d directories before depth rejection", len(reader.loads))
	}
}

func TestMeasureSelectionReportsSmallOnlyAfterCompleteDiscovery(t *testing.T) {
	share := transferID[catalog.ShareInstance](93)
	root := transferID[catalog.DirectoryID](94)
	child := transferID[catalog.DirectoryID](95)
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root: jobSnapshot(t, share, root, 1,
			jobEntry(t, transferID[catalog.FileID](1), "root-file", 3),
			jobDirectoryEntry(t, child, "child"),
		),
		child: jobSnapshot(t, share, child, 2,
			jobEntry(t, transferID[catalog.FileID](2), "child-file", 5),
		),
	}}
	rules, _ := NewSelectionRules(true, nil)

	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if measure.Class() != SelectionSmall || !measure.DiscoveryTerminalSuccess ||
		measure.DiscoveredFiles != 2 || measure.DiscoveredBytes != 8 {
		t.Fatalf("measure=%+v class=%v", measure, measure.Class())
	}
	if !slices.Equal(reader.loads, []catalog.DirectoryID{root, child}) {
		t.Fatalf("loads=%x", reader.loads)
	}
}

func TestMeasureSelectionContinuesSiblingsAfterNonfatalDirectoryFailure(t *testing.T) {
	share := transferID[catalog.ShareInstance](96)
	root := transferID[catalog.DirectoryID](97)
	failed := transferID[catalog.DirectoryID](98)
	healthy := transferID[catalog.DirectoryID](99)
	directoryErr := errors.New("directory temporarily unavailable")
	reader := &selectionMeasureCatalog{
		snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root: jobSnapshot(t, share, root, 1,
				jobDirectoryEntry(t, failed, "a-failed"),
				jobDirectoryEntry(t, healthy, "b-healthy"),
			),
			healthy: jobSnapshot(t, share, healthy, 2,
				jobEntry(t, transferID[catalog.FileID](3), "survivor", 7),
			),
		},
		failures: map[catalog.DirectoryID]error{failed: selectionDirectoryFailure{directoryErr}},
	}
	rules, _ := NewSelectionRules(true, nil)

	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if measure.Class() != SelectionUnknown || measure.DiscoveryTerminalSuccess ||
		measure.DiscoveredFiles != 1 || measure.DiscoveredBytes != 7 {
		t.Fatalf("measure=%+v class=%v", measure, measure.Class())
	}
	if !slices.Equal(reader.loads, []catalog.DirectoryID{root, failed, healthy}) {
		t.Fatalf("loads=%x", reader.loads)
	}
}

func TestMeasureSelectionReturnsFatalSessionAndCatalogIdentityErrors(t *testing.T) {
	share := transferID[catalog.ShareInstance](100)
	root := transferID[catalog.DirectoryID](101)
	failed := transferID[catalog.DirectoryID](102)
	later := transferID[catalog.DirectoryID](103)
	rules, _ := NewSelectionRules(true, nil)

	t.Run("session failure", func(t *testing.T) {
		cause := errors.New("authenticated session ended")
		reader := &selectionMeasureCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1,
					jobDirectoryEntry(t, failed, "a-failed"),
					jobDirectoryEntry(t, later, "b-later"),
				),
			},
			failures: map[catalog.DirectoryID]error{failed: NewSessionFailure(cause)},
		}
		measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
		})
		if !errors.Is(err, cause) || measure.Class() != SelectionUnknown {
			t.Fatalf("measure=%+v err=%v", measure, err)
		}
		if !slices.Equal(reader.loads, []catalog.DirectoryID{root, failed}) {
			t.Fatalf("loads=%x", reader.loads)
		}
	})

	t.Run("foreign snapshot", func(t *testing.T) {
		foreignShare := transferID[catalog.ShareInstance](104)
		reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root: jobSnapshot(t, foreignShare, root, 1),
		}}
		_, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
		})
		if !errors.Is(err, ErrCatalogIdentity) || !isSessionFailure(err) {
			t.Fatalf("identity error=%v", err)
		}
	})
}

func TestMeasureSelectionRejectsInvalidConfigurationAndCancellation(t *testing.T) {
	if _, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{}); !errors.Is(err, ErrInvalidSelectionMeasurement) {
		t.Fatalf("invalid configuration error=%v", err)
	}
	share := transferID[catalog.ShareInstance](105)
	root := transferID[catalog.DirectoryID](106)
	rules, _ := NewSelectionRules(false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MeasureSelection(ctx, SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: &selectionMeasureCatalog{},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled measurement error=%v", err)
	}
}
