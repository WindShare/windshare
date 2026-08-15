package ordinaryoutput

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/catalogwalk"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestShapeContractsAreClosed(t *testing.T) {
	if !ShapeSingleFile.Valid() || !ShapeSyntheticSelection.Valid() || ShapeKind(0).Valid() || ShapeKind(5).Valid() {
		t.Fatal("shape kind is not closed")
	}
	if !ShapeFallbackNone.Valid() || !ShapeFallbackIncompleteGeneration.Valid() ||
		ShapeFallbackReason(ShapeFallbackIncompleteGeneration+1).Valid() {
		t.Fatal("shape fallback reason is not closed")
	}
	if (ShapeDecision{}).Valid() {
		t.Fatal("zero shape decision is valid")
	}
	if _, err := NewSyntheticSelectionShape(ShapeFallbackNone); !errors.Is(err, ErrInvalidShapeResolution) {
		t.Fatalf("synthetic proof accepted no fallback: %v", err)
	}
}

func TestResolveShapeWholeShareProofsAndFreeze(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	file := shapeID[catalog.FileID](3)
	directory := shapeID[catalog.DirectoryID](4)
	selection := wholeSelection(t, share, root)

	tests := []struct {
		name       string
		entries    []catalog.Entry
		wantKind   ShapeKind
		wantName   string
		wantReason ShapeFallbackReason
	}{
		{
			name:     "single file",
			entries:  []catalog.Entry{shapeFileEntry(t, file, "file.bin")},
			wantKind: ShapeSingleFile, wantName: "file.bin",
		},
		{
			name:     "single directory",
			entries:  []catalog.Entry{shapeDirectoryEntry(t, directory, "photos")},
			wantKind: ShapeCompleteDirectory, wantName: "photos",
		},
		{
			name:       "empty share",
			wantKind:   ShapeSyntheticSelection,
			wantName:   "windshare",
			wantReason: ShapeFallbackMultipleRoots,
		},
		{
			name: "multiple roots",
			entries: []catalog.Entry{
				shapeDirectoryEntry(t, directory, "photos"),
				shapeFileEntry(t, file, "readme.txt"),
			},
			wantKind: ShapeSyntheticSelection, wantName: "windshare",
			wantReason: ShapeFallbackMultipleRoots,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
				root: test.entries,
			})
			var traces []ShapeTrace
			decision, err := ResolveShape(
				context.Background(), source, selection, DefaultShapeProbeBudgetV1,
				ShapeTraceFunc(func(event ShapeTrace) { traces = append(traces, event) }),
			)
			if err != nil || decision.Kind() != test.wantKind ||
				decision.PreferredName() != test.wantName ||
				decision.FallbackReason() != test.wantReason || !decision.Valid() {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			if len(traces) != 2 || traces[0].Stage != ShapeProbeStarted ||
				traces[1].Kind != test.wantKind ||
				traces[1].SelectionDigest == ([32]byte{}) {
				t.Fatalf("traces=%+v", traces)
			}
			// A later root generation cannot mutate the returned scalar proof.
			source.entries[root] = []catalog.Entry{
				shapeFileEntry(t, shapeID[catalog.FileID](8), "later.bin"),
			}
			if decision.Kind() != test.wantKind || decision.PreferredName() != test.wantName {
				t.Fatalf("frozen decision changed after source mutation: %+v", decision)
			}
		})
	}
}

func TestResolveShapePathProofs(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	nested := shapeID[catalog.DirectoryID](4)
	other := shapeID[catalog.DirectoryID](5)
	image := shapeID[catalog.FileID](6)
	second := shapeID[catalog.FileID](7)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
			shapeDirectoryEntry(t, other, "work"),
		},
		photos: {
			shapeFileEntry(t, image, "image.jpg"),
			shapeDirectoryEntry(t, nested, "nested"),
		},
		nested: {
			shapeFileEntry(t, second, "second.jpg"),
		},
		other: {
			shapeFileEntry(t, shapeID[catalog.FileID](8), "notes.txt"),
		},
	})

	tests := []struct {
		name       string
		targets    []string
		wantKind   ShapeKind
		wantPath   string
		wantName   string
		wantReason ShapeFallbackReason
	}{
		{
			name: "direct file", targets: []string{"photos/image.jpg"},
			wantKind: ShapeSingleFile, wantPath: "photos/image.jpg", wantName: "image.jpg",
		},
		{
			name:     "direct directory ignores redundant descendant",
			targets:  []string{"photos", "photos/nested/second.jpg"},
			wantKind: ShapeCompleteDirectory, wantPath: "photos", wantName: "photos",
		},
		{
			name:     "partial directory uses nearest authenticated real ancestor",
			targets:  []string{"photos/image.jpg", "photos/nested/second.jpg"},
			wantKind: ShapePartialDirectory, wantPath: "photos", wantName: "photos-selection",
		},
		{
			name:     "synthetic nearest ancestor",
			targets:  []string{"photos/image.jpg", "work/notes.txt"},
			wantKind: ShapeSyntheticSelection, wantName: "windshare",
			wantReason: ShapeFallbackSyntheticNearestAncestor,
		},
		{
			name:     "unresolved target stays for later job",
			targets:  []string{"photos/missing.jpg"},
			wantKind: ShapeSyntheticSelection, wantName: "windshare",
			wantReason: ShapeFallbackUnresolvedTarget,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection := pathSelection(t, share, root, test.targets)
			decision, err := ResolveShape(
				context.Background(), source, selection, DefaultShapeProbeBudgetV1,
				nil,
			)
			if err != nil || decision.Kind() != test.wantKind ||
				decision.SourcePath() != test.wantPath ||
				decision.PreferredName() != test.wantName ||
				decision.FallbackReason() != test.wantReason {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
		})
	}
}

func TestResolveShapePartialDirectoryUsesCanonicalTruncatedName(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	directory := shapeID[catalog.DirectoryID](3)
	first := shapeID[catalog.FileID](4)
	second := shapeID[catalog.FileID](5)
	name := strings.Repeat("a", catalog.MaxNameBytes)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, directory, name),
		},
		directory: {
			shapeFileEntry(t, first, "first.bin"),
			shapeFileEntry(t, second, "second.bin"),
		},
	})
	decision, err := ResolveShape(
		context.Background(),
		source,
		pathSelection(t, share, root, []string{name + "/first.bin", name + "/second.bin"}),
		DefaultShapeProbeBudgetV1,
		nil,
	)
	if err != nil || decision.Kind() != ShapePartialDirectory ||
		decision.SourcePath() != name || len([]byte(decision.PreferredName())) != receivecontract.MaxResultComponentBytes ||
		!strings.HasSuffix(decision.PreferredName(), receivecontract.PartialSelectionSuffix) {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestResolveShapePathProofReadsEachParentGenerationOnce(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	first := shapeID[catalog.FileID](4)
	second := shapeID[catalog.FileID](5)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
		},
		photos: {
			shapeFileEntry(t, first, "first.jpg"),
			shapeFileEntry(t, second, "second.jpg"),
		},
	})
	decision, err := ResolveShape(
		context.Background(),
		source,
		pathSelection(t, share, root, []string{"photos/first.jpg", "photos/second.jpg"}),
		DefaultShapeProbeBudgetV1,
		nil,
	)
	if err != nil || decision.Kind() != ShapePartialDirectory ||
		decision.SourcePath() != "photos" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	if source.opens[root] != 1 || source.opens[photos] != 1 {
		t.Fatalf("proof mixed parent generations: opens=%v", source.opens)
	}
}

func TestResolveShapeOpaqueProofIgnoresAncestryHintsAndAuthenticatesExclusions(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	nested := shapeID[catalog.DirectoryID](4)
	image := shapeID[catalog.FileID](5)
	other := shapeID[catalog.FileID](6)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
		},
		photos: {
			shapeFileEntry(t, image, "image.jpg"),
			shapeDirectoryEntry(t, nested, "nested"),
		},
		nested: {
			shapeFileEntry(t, other, "other.jpg"),
		},
	})

	t.Run("selected file", func(t *testing.T) {
		target, _ := NewOpaqueFileTarget(other, true)
		selection := opaqueSelection(t, share, root, false, []OpaqueSelectionTarget{target})
		decision, err := ResolveShape(
			context.Background(), source, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapeSingleFile ||
			decision.SourcePath() != "photos/nested/other.jpg" {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
		if source.opens[photos] == 0 || source.opens[nested] == 0 {
			t.Fatalf("opaque proof trusted an ancestry hint instead of searching: opens=%v", source.opens)
		}
	})

	t.Run("selected directory needs no descendant manifest", func(t *testing.T) {
		selected, _ := NewOpaqueDirectoryTarget(photos, true)
		selection := opaqueSelection(
			t, share, root, false, []OpaqueSelectionTarget{selected},
		)
		isolatedSource := newShapeCatalog(t, share, source.entries)
		decision, err := ResolveShape(
			context.Background(), isolatedSource, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapeCompleteDirectory ||
			decision.SourcePath() != "photos" || isolatedSource.opens[photos] != 0 {
			t.Fatalf("decision=%+v opens=%v err=%v", decision, isolatedSource.opens, err)
		}
	})

	t.Run("directory exclusion makes partial", func(t *testing.T) {
		selected, _ := NewOpaqueDirectoryTarget(photos, true)
		excluded, _ := NewOpaqueFileTarget(other, false)
		selection := opaqueSelection(
			t, share, root, false, []OpaqueSelectionTarget{selected, excluded},
		)
		decision, err := ResolveShape(
			context.Background(), source, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapePartialDirectory ||
			decision.SourcePath() != "photos" ||
			decision.PreferredName() != "photos-selection" {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})

	t.Run("selected file overrides ancestor exclusion", func(t *testing.T) {
		selected, _ := NewOpaqueFileTarget(image, true)
		excluded, _ := NewOpaqueDirectoryTarget(photos, false)
		selection := opaqueSelection(
			t, share, root, false, []OpaqueSelectionTarget{selected, excluded},
		)
		decision, err := ResolveShape(
			context.Background(), source, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapeSingleFile ||
			decision.SourcePath() != "photos/image.jpg" {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})

	t.Run("inherited selection authenticates exclusion before freezing partial", func(t *testing.T) {
		excluded, _ := NewOpaqueFileTarget(other, false)
		selection := opaqueSelection(
			t, share, root, true, []OpaqueSelectionTarget{excluded},
		)
		isolatedSource := newShapeCatalog(t, share, source.entries)
		decision, err := ResolveShape(
			context.Background(), isolatedSource, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapePartialDirectory ||
			decision.SourcePath() != "photos" || isolatedSource.opens[nested] == 0 {
			t.Fatalf("decision=%+v opens=%v err=%v", decision, isolatedSource.opens, err)
		}
	})

	t.Run("inherited multiple roots remains synthetic", func(t *testing.T) {
		secondRoot := shapeID[catalog.FileID](7)
		excluded, _ := NewOpaqueFileTarget(image, false)
		selection := opaqueSelection(
			t, share, root, true, []OpaqueSelectionTarget{excluded},
		)
		isolatedSource := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
			root: {
				shapeDirectoryEntry(t, photos, "photos"),
				shapeFileEntry(t, secondRoot, "readme.txt"),
			},
			photos: source.entries[photos],
			nested: source.entries[nested],
		})
		decision, err := ResolveShape(
			context.Background(), isolatedSource, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err != nil || decision.Kind() != ShapeSyntheticSelection ||
			decision.FallbackReason() != ShapeFallbackSyntheticNearestAncestor {
			t.Fatalf("decision=%+v opens=%v err=%v", decision, isolatedSource.opens, err)
		}
	})
}

func TestResolveShapeOpaqueExclusionOutsideSelectedDirectoryKeepsCompleteShape(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	image := shapeID[catalog.FileID](4)
	other := shapeID[catalog.FileID](5)
	selected, _ := NewOpaqueDirectoryTarget(photos, true)
	excluded, _ := NewOpaqueFileTarget(other, false)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
			shapeFileEntry(t, other, "notes.txt"),
		},
		photos: {
			shapeFileEntry(t, image, "image.jpg"),
		},
	})
	decision, err := ResolveShape(
		context.Background(), source,
		opaqueSelection(t, share, root, false, []OpaqueSelectionTarget{selected, excluded}),
		DefaultShapeProbeBudgetV1, nil,
	)
	if err != nil || decision.Kind() != ShapeCompleteDirectory ||
		decision.SourcePath() != "photos" || source.opens[photos] != 0 {
		t.Fatalf("decision=%+v opens=%v err=%v", decision, source.opens, err)
	}
}

func TestResolveShapeOpaqueDepthBudgetDoesNotOpenOutOfBudgetDirectory(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	nested := shapeID[catalog.DirectoryID](4)
	file := shapeID[catalog.FileID](5)
	target, _ := NewOpaqueFileTarget(file, true)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
		},
		photos: {
			shapeDirectoryEntry(t, nested, "nested"),
		},
		nested: {
			shapeFileEntry(t, file, "file.bin"),
		},
	})
	decision, err := ResolveShape(
		context.Background(), source,
		opaqueSelection(t, share, root, false, []OpaqueSelectionTarget{target}),
		shapeBudget(t, 4, 4, 10, 1<<20, 1), nil,
	)
	if err != nil || decision.Kind() != ShapeSyntheticSelection ||
		decision.FallbackReason() != ShapeFallbackDepthBudget || source.opens[nested] != 0 {
		t.Fatalf("decision=%+v opens=%v err=%v", decision, source.opens, err)
	}
}

func TestResolveShapeEveryBudgetAxisFallsBackAndClosesCursor(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	photos := shapeID[catalog.DirectoryID](3)
	nested := shapeID[catalog.DirectoryID](4)
	file := shapeID[catalog.FileID](5)
	entries := map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, photos, "photos"),
		},
		photos: {
			shapeDirectoryEntry(t, nested, "nested"),
		},
		nested: {
			shapeFileEntry(t, file, "file.bin"),
		},
	}

	tests := []struct {
		name       string
		budget     ShapeProbeBudget
		selection  Selection
		wantReason ShapeFallbackReason
	}{
		{
			name:       "page",
			budget:     shapeBudget(t, 4, 1, 10, 1<<20, 4),
			selection:  pathSelection(t, share, root, []string{"photos/nested/file.bin"}),
			wantReason: ShapeFallbackPageBudget,
		},
		{
			name:       "entry",
			budget:     shapeBudget(t, 4, 4, 1, 1<<20, 4),
			selection:  pathSelection(t, share, root, []string{"photos/nested/file.bin"}),
			wantReason: ShapeFallbackEntryBudget,
		},
		{
			name:       "metadata",
			budget:     shapeBudget(t, 4, 4, 10, 1, 4),
			selection:  pathSelection(t, share, root, []string{"photos/nested/file.bin"}),
			wantReason: ShapeFallbackMetadataBudget,
		},
		{
			name:       "depth",
			budget:     shapeBudget(t, 4, 4, 10, 1<<20, 2),
			selection:  pathSelection(t, share, root, []string{"photos/nested/file.bin"}),
			wantReason: ShapeFallbackDepthBudget,
		},
		{
			name:       "request",
			budget:     shapeBudget(t, 2, 4, 10, 1<<20, 4),
			selection:  pathSelection(t, share, root, []string{"photos/nested/file.bin"}),
			wantReason: ShapeFallbackDirectoryRequestBudget,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newShapeCatalog(t, share, entries)
			decision, err := ResolveShape(
				context.Background(), source, test.selection, test.budget,
				nil,
			)
			if err != nil || decision.Kind() != ShapeSyntheticSelection ||
				decision.FallbackReason() != test.wantReason {
				t.Fatalf("decision=%+v err=%v", decision, err)
			}
			for _, cursor := range source.cursors {
				if !cursor.closed {
					t.Fatalf("budget fallback left cursor open")
				}
			}
		})
	}
}

func TestResolveShapeIncompleteGenerationFallsBack(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	file := shapeID[catalog.FileID](3)
	page := shapePageWithOmissions(share, root, []catalog.Entry{
		shapeFileEntry(t, file, "file.bin"),
	}, 1, 1)
	source := &staticShapeCatalog{
		cursors: map[catalog.DirectoryID]*shapeCursor{
			root: {pages: []catalog.CatalogPage{page}},
		},
	}
	decision, err := ResolveShape(
		context.Background(), source, wholeSelection(t, share, root),
		DefaultShapeProbeBudgetV1, nil,
	)
	if err != nil || decision.Kind() != ShapeSyntheticSelection ||
		decision.FallbackReason() != ShapeFallbackIncompleteGeneration ||
		!source.cursors[root].closed {
		t.Fatalf("decision=%+v closed=%v err=%v", decision, source.cursors[root].closed, err)
	}
}

func TestResolveShapeAuthenticationAndSessionErrorsAreNotFallback(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	selection := wholeSelection(t, share, root)

	t.Run("transport", func(t *testing.T) {
		sessionErr := errors.New("session failed")
		source := &shapeCatalog{
			share: share, entries: map[catalog.DirectoryID][]catalog.Entry{},
			openErr: map[catalog.DirectoryID]error{root: sessionErr},
		}
		decision, err := ResolveShape(
			context.Background(), source, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if !errors.Is(err, sessionErr) || decision.Valid() {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})

	t.Run("integrity", func(t *testing.T) {
		wrongShare := shapeID[catalog.ShareInstance](9)
		source := newShapeCatalog(t, wrongShare, map[catalog.DirectoryID][]catalog.Entry{
			root: {shapeFileEntry(t, shapeID[catalog.FileID](3), "file.bin")},
		})
		decision, err := ResolveShape(
			context.Background(), source, selection, DefaultShapeProbeBudgetV1,
			nil,
		)
		if err == nil || decision.Valid() {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})
}

func TestResolveShapeRejectsCrossBranchNodeIdentityAlias(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	left := shapeID[catalog.DirectoryID](3)
	right := shapeID[catalog.DirectoryID](4)
	aliased := shapeID[catalog.FileID](5)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {
			shapeDirectoryEntry(t, left, "left"),
			shapeDirectoryEntry(t, right, "right"),
		},
		left: {
			shapeFileEntry(t, aliased, "first.bin"),
		},
		right: {
			shapeFileEntry(t, aliased, "second.bin"),
		},
	})
	decision, err := ResolveShape(
		context.Background(),
		source,
		pathSelection(t, share, root, []string{"left/first.bin", "right/second.bin"}),
		DefaultShapeProbeBudgetV1,
		nil,
	)
	if !errors.Is(err, catalogwalk.ErrTerminalGenerationIntegrity) || decision.Valid() {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestShapeTraceIsRedactedAndSessionBound(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	file := shapeID[catalog.FileID](3)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {shapeFileEntry(t, file, "secret-file.bin")},
	})
	session := shapeID[protocolsession.ProtocolSessionID](9)
	var traces []ShapeTrace
	decision, err := ResolveShape(
		context.Background(), source, wholeSelection(t, share, root),
		DefaultShapeProbeBudgetV1,
		BindShapeTracerToSession(session, ShapeTraceFunc(func(event ShapeTrace) {
			traces = append(traces, event)
		})),
	)
	if err != nil || decision.Kind() != ShapeSingleFile || len(traces) != 2 {
		t.Fatalf("decision=%+v traces=%+v err=%v", decision, traces, err)
	}
	for _, event := range traces {
		if event.ProtocolSessionID != session || event.SelectionDigest == ([32]byte{}) ||
			event.DirectoryRequests > DefaultShapeProbeBudgetV1.DirectoryRequests() ||
			event.AuthenticatedPages > DefaultShapeProbeBudgetV1.AuthenticatedPages() ||
			event.AuthenticatedEntries > DefaultShapeProbeBudgetV1.Entries() ||
			event.AuthenticatedMetadataBytes > DefaultShapeProbeBudgetV1.AuthenticatedMetadataBytes() {
			t.Fatalf("unbounded or uncorrelated trace=%+v", event)
		}
	}
}

func TestShapeTracePanicCannotChangeDecision(t *testing.T) {
	share := shapeID[catalog.ShareInstance](1)
	root := shapeID[catalog.DirectoryID](2)
	source := newShapeCatalog(t, share, map[catalog.DirectoryID][]catalog.Entry{
		root: {shapeFileEntry(t, shapeID[catalog.FileID](3), "file.bin")},
	})
	decision, err := ResolveShape(
		context.Background(), source, wholeSelection(t, share, root),
		DefaultShapeProbeBudgetV1,
		ShapeTraceFunc(func(ShapeTrace) { panic("diagnostic") }),
	)
	if err != nil || decision.Kind() != ShapeSingleFile {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

type staticShapeCatalog struct {
	cursors map[catalog.DirectoryID]*shapeCursor
}

func (source *staticShapeCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	cursor := source.cursors[directory]
	if cursor == nil {
		return nil, errors.New("directory missing")
	}
	return cursor, nil
}

type shapeCatalog struct {
	share   catalog.ShareInstance
	entries map[catalog.DirectoryID][]catalog.Entry
	openErr map[catalog.DirectoryID]error
	opens   map[catalog.DirectoryID]int
	cursors []*shapeCursor
}

func newShapeCatalog(
	t *testing.T,
	share catalog.ShareInstance,
	entries map[catalog.DirectoryID][]catalog.Entry,
) *shapeCatalog {
	t.Helper()
	return &shapeCatalog{
		share: share, entries: entries, openErr: make(map[catalog.DirectoryID]error),
		opens: make(map[catalog.DirectoryID]int),
	}
}

func (source *shapeCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if err := source.openErr[directory]; err != nil {
		return nil, err
	}
	source.opens[directory]++
	entries, exists := source.entries[directory]
	if !exists {
		return nil, errors.New("directory missing")
	}
	sorted := append([]catalog.Entry(nil), entries...)
	sort.Slice(sorted, func(left, right int) bool {
		return sorted[left].Name() < sorted[right].Name()
	})
	page := shapePage(source.share, directory, sorted, source.opens[directory])
	cursor := &shapeCursor{pages: []catalog.CatalogPage{page}}
	source.cursors = append(source.cursors, cursor)
	return cursor, nil
}

type shapeCursor struct {
	pages  []catalog.CatalogPage
	index  int
	closed bool
}

func (cursor *shapeCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	if cursor.index >= len(cursor.pages) {
		return catalog.CatalogPage{}, false, nil
	}
	page := cursor.pages[cursor.index]
	cursor.index++
	return page, true, nil
}

func (cursor *shapeCursor) Close() error {
	cursor.closed = true
	return nil
}

func shapePage(
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	entries []catalog.Entry,
	generationSeed int,
) catalog.CatalogPage {
	return shapePageWithOmissions(share, directory, entries, generationSeed, 0)
}

func shapePageWithOmissions(
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	entries []catalog.Entry,
	generationSeed int,
	omitted uint64,
) catalog.CatalogPage {
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory,
		Generation: shapeID[catalog.DirectoryGeneration](byte(20 + generationSeed)),
		Entries:    entries, Terminal: true, OmittedCount: omitted,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		raw := make([]byte, catalog.PageCommitmentBytes)
		for index := range raw {
			raw[index] = byte(40 + generationSeed)
		}
		return catalog.NewPageCommitment(raw)
	}))
	if err != nil {
		panic(err)
	}
	return page
}

func shapeFileEntry(t *testing.T, file catalog.FileID, name string) catalog.Entry {
	t.Helper()
	entry, err := catalog.NewFileEntry(file, name, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func shapeDirectoryEntry(
	t *testing.T,
	directory catalog.DirectoryID,
	name string,
) catalog.Entry {
	t.Helper()
	entry, err := catalog.NewDirectoryEntry(directory, name, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func wholeSelection(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
) Selection {
	t.Helper()
	selection, err := NewWholeShareSelection(share, root, shapeDigest(1))
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func pathSelection(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	targets []string,
) Selection {
	t.Helper()
	selection, err := NewCatalogPathSelection(share, root, shapeDigest(2), targets)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func opaqueSelection(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	defaultSelected bool,
	targets []OpaqueSelectionTarget,
) Selection {
	t.Helper()
	selection, err := NewOpaqueNodeSelection(
		share, root, shapeDigest(3), defaultSelected, targets,
	)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func shapeBudget(
	t *testing.T,
	requests, pages, entries uint32,
	metadata uint64,
	depth uint32,
) ShapeProbeBudget {
	t.Helper()
	budget, ok := NewShapeProbeBudget(requests, pages, entries, metadata, depth)
	if !ok {
		t.Fatal("invalid test budget")
	}
	return budget
}

func shapeDigest(seed byte) [32]byte {
	var digest [32]byte
	for index := range digest {
		digest[index] = seed
	}
	return digest
}

func shapeID[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed
	}
	return value
}
