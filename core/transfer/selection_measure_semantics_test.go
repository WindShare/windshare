package transfer

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type selectionNilReleaseCatalog struct {
	snapshot catalog.DirectorySnapshot
}

func (catalogSource selectionNilReleaseCatalog) LoadDirectory(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectorySnapshot, error) {
	return catalogSource.snapshot, nil
}

func (catalogSource selectionNilReleaseCatalog) AcquireDirectory(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	return catalogSource.snapshot, nil, nil
}

func TestMeasureSelectionEmptyIntentAvoidsCatalogAndPublishesTerminalObservation(t *testing.T) {
	share := transferID[catalog.ShareInstance](131)
	root := transferID[catalog.DirectoryID](132)
	rules, err := NewSelectionRules(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &selectionMeasureCatalog{}
	var observations []SelectionMeasure
	measure, err := measureSelection(
		context.Background(),
		SelectionMeasurementConfig{
			ShareInstance: share,
			SyntheticRoot: root,
			Rules:         rules,
			Catalog:       reader,
		},
		func(current SelectionMeasure) { observations = append(observations, current) },
	)
	if err != nil || measure.Class() != SelectionSmall || !measure.DiscoveryTerminalSuccess {
		t.Fatalf("empty selection measure = %+v, error = %v", measure, err)
	}
	if len(reader.loads) != 0 || !slices.Equal(observations, []SelectionMeasure{measure}) {
		t.Fatalf("empty selection catalog loads = %v, observations = %+v", reader.loads, observations)
	}
}

func TestMeasureSelectionMissingTargetAndNilLeaseReleaseFailClosed(t *testing.T) {
	share := transferID[catalog.ShareInstance](133)
	root := transferID[catalog.DirectoryID](134)
	missing := transferID[catalog.FileID](135)
	rules, err := NewSelectionRules(false, []SelectionOverride{{
		FileID: missing, Selected: true, Ancestors: []catalog.DirectoryID{root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot := jobSnapshot(t, share, root, 1)
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: rootSnapshot}}
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if !errors.Is(err, ErrSelectionTargetMissing) || measure.DiscoveryTerminalSuccess {
		t.Fatalf("missing target measure = %+v, error = %v", measure, err)
	}

	all, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         all,
		Catalog:       selectionNilReleaseCatalog{snapshot: rootSnapshot},
	})
	if !errors.Is(err, ErrCatalogLeaseContract) || !isJobTerminalError(err) {
		t.Fatalf("nil catalog lease release error = %v", err)
	}
}

func TestMeasureSelectionMatchesTypedTargetsWithoutTrustingAncestryHints(t *testing.T) {
	share := transferID[catalog.ShareInstance](136)
	root := transferID[catalog.DirectoryID](137)
	selected := transferID[catalog.FileID](138)
	unselected := transferID[catalog.FileID](139)
	unrelated := transferID[catalog.DirectoryID](140)
	rules, err := NewSelectionRules(false, []SelectionOverride{{
		FileID: selected, Selected: true, Ancestors: []catalog.DirectoryID{root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root: jobSnapshot(
			t,
			share,
			root,
			1,
			jobEntry(t, unselected, "a-unselected.bin", 2),
			jobEntry(t, selected, "b-selected.bin", 3),
			jobDirectoryEntry(t, unrelated, "c-unrelated"),
		),
		unrelated: jobSnapshot(t, share, unrelated, 2),
	}}
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil || measure.Class() != SelectionSmall || measure.DiscoveredFiles != 1 || measure.DiscoveredBytes != 3 {
		t.Fatalf("typed selection measure = %+v, error = %v", measure, err)
	}
	if !slices.Equal(reader.loads, []catalog.DirectoryID{root, unrelated}) {
		t.Fatalf("node-ID ancestry hint became pruning authority: loads = %v", reader.loads)
	}
}

func TestMeasureSelectionMatchesDirectoryAndPathTargets(t *testing.T) {
	share := transferID[catalog.ShareInstance](141)
	root := transferID[catalog.DirectoryID](142)
	child := transferID[catalog.DirectoryID](143)
	unrelated := transferID[catalog.DirectoryID](147)
	snapshots := map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root: jobSnapshot(
			t,
			share,
			root,
			1,
			jobDirectoryEntry(t, child, "folder"),
			jobDirectoryEntry(t, unrelated, "unrelated"),
		),
		child:     jobSnapshot(t, share, child, 2),
		unrelated: jobSnapshot(t, share, unrelated, 3),
	}

	directoryRules, err := NewSelectionRules(false, []SelectionOverride{{
		DirectoryID: child, Selected: true, Ancestors: []catalog.DirectoryID{root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	reader := &selectionMeasureCatalog{snapshots: snapshots}
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: directoryRules, Catalog: reader,
	})
	if err != nil || measure.Class() != SelectionSmall ||
		!slices.Equal(reader.loads, []catalog.DirectoryID{root, child, unrelated}) {
		t.Fatalf("directory target measure = %+v, loads = %v, error = %v", measure, reader.loads, err)
	}

	pathRules, err := NewPathSelectionRules([]string{"folder"})
	if err != nil {
		t.Fatal(err)
	}
	reader = &selectionMeasureCatalog{snapshots: snapshots}
	measure, err = MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: pathRules, Catalog: reader,
	})
	if err != nil || measure.Class() != SelectionSmall || !slices.Equal(reader.loads, []catalog.DirectoryID{root, child}) {
		t.Fatalf("path target measure = %+v, loads = %v, error = %v", measure, reader.loads, err)
	}
}

func TestSelectionMeasurerCancellationAndPageBoundsAreTerminal(t *testing.T) {
	share := transferID[catalog.ShareInstance](144)
	root := transferID[catalog.DirectoryID](145)
	snapshot := jobSnapshot(
		t,
		share,
		root,
		1,
		jobEntry(t, transferID[catalog.FileID](146), "file.bin", 1),
	)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	measurer := selectionMeasurer{
		share:              share,
		rules:              rules,
		catalog:            &selectionMeasureCatalog{},
		claims:             newSelectionIdentityClaims(root),
		matchedPaths:       make(map[string]struct{}),
		matchedDirectories: make(map[catalog.DirectoryID]struct{}),
		matchedFiles:       make(map[catalog.FileID]struct{}),
	}
	walk, err := measurer.startDirectoryWalk(snapshot, root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := measurer.acquireDirectory(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition error = %v", err)
	}
	if _, err := measurer.measureDirectoryFiles(ctx, walk); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled file measurement error = %v", err)
	}
	if _, err := measurer.walkDirectoryChildren(ctx, walk); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled child walk error = %v", err)
	}
	if _, err := selectionMeasurementPage(snapshot, snapshot.PageCount()); !errors.Is(err, ErrCatalogIdentity) || !isSessionFailure(err) {
		t.Fatalf("out-of-range measurement page error = %v", err)
	}
}

func TestMeasureSelectionMatchesAuthenticatedFilePath(t *testing.T) {
	share := transferID[catalog.ShareInstance](148)
	root := transferID[catalog.DirectoryID](149)
	child := transferID[catalog.DirectoryID](150)
	file := transferID[catalog.FileID](151)
	rules, err := NewPathSelectionRules([]string{"folder/file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
		root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, child, "folder")),
		child: jobSnapshot(t, share, child, 2, jobEntry(t, file, "file.bin", 7)),
	}}
	measure, err := MeasureSelection(context.Background(), SelectionMeasurementConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
	})
	if err != nil || measure.Class() != SelectionSmall || measure.DiscoveredFiles != 1 || measure.DiscoveredBytes != 7 {
		t.Fatalf("path-selected file measure = %+v, error = %v", measure, err)
	}
}

func TestMeasureSelectionRejectsDuplicateIdentityAndOverdeepFilePath(t *testing.T) {
	t.Run("duplicate node identity", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](152)
		root := transferID[catalog.DirectoryID](153)
		file := transferID[catalog.FileID](154)
		child := transferID[catalog.DirectoryID](156)
		rules, err := NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		reader := &selectionMeasureCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root: jobSnapshot(
				t,
				share,
				root,
				1,
				jobEntry(t, file, "first.bin", 1),
				jobDirectoryEntry(t, child, "child"),
			),
			child: jobSnapshot(t, share, child, 2, jobEntry(t, file, "second.bin", 1)),
		}}
		_, err = MeasureSelection(context.Background(), SelectionMeasurementConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: reader,
		})
		if !errors.Is(err, ErrCatalogIdentity) || !isSessionFailure(err) {
			t.Fatalf("duplicate selection identity error = %v", err)
		}
	})

	t.Run("file exceeds authenticated path depth", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](155)
		root := replayDirectoryID(2_000)
		snapshots := make(map[catalog.DirectoryID]catalog.DirectorySnapshot, catalog.MaxPathDepth+1)
		current := root
		for index := range catalog.MaxPathDepth {
			child := replayDirectoryID(2_001 + index)
			snapshots[current] = jobSnapshot(
				t,
				share,
				current,
				1,
				jobDirectoryEntry(t, child, "d"),
			)
			current = child
		}
		snapshots[current] = jobSnapshot(
			t,
			share,
			current,
			1,
			jobEntry(t, replayFileID(3_000), "file.bin", 1),
		)
		rules, err := NewSelectionRules(true, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = MeasureSelection(context.Background(), SelectionMeasurementConfig{
			ShareInstance: share,
			SyntheticRoot: root,
			Rules:         rules,
			Catalog:       &selectionMeasureCatalog{snapshots: snapshots},
		})
		if !errors.Is(err, ErrCatalogIdentity) || !isSessionFailure(err) {
			t.Fatalf("overdeep selected file error = %v", err)
		}
	})
}
