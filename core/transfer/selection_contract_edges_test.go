package transfer

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectionRulesRejectInvalidAndCollectivelyOversizedPaths(t *testing.T) {
	if _, err := NewPathSelectionRules([]string{"../escape"}); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("invalid catalog path error = %v", err)
	}

	// Each target is individually valid. The aggregate budget is an independent
	// admission boundary so a large CLI selection cannot bypass catalog limits.
	paths := make([]string, MaxSelectionPathTargets)
	component := strings.Repeat("a", catalog.MaxNameBytes-3)
	for index := range paths {
		paths[index] = fmt.Sprintf("%04d/%s", index, component)
	}
	if _, err := NewPathSelectionRules(paths); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("aggregate path budget error = %v", err)
	}
}

func TestSelectionRulesRejectOversizedAdvisoryAncestry(t *testing.T) {
	file := transferID[catalog.FileID](231)
	ancestors := make([]catalog.DirectoryID, catalog.MaxPathDepth+1)
	for index := range ancestors {
		ancestors[index][0] = byte(index/255 + 1)
		ancestors[index][1] = byte(index%255 + 1)
	}
	if _, err := NewSelectionRules(false, []SelectionOverride{{
		FileID: file, Selected: true, Ancestors: ancestors,
	}}); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("oversized ancestry error = %v", err)
	}
}

func TestSelectionMissingNodeTargetsHaveDeterministicIdentityOrder(t *testing.T) {
	directoryHigh := transferID[catalog.DirectoryID](241)
	directoryLow := transferID[catalog.DirectoryID](240)
	fileHigh := transferID[catalog.FileID](243)
	fileLow := transferID[catalog.FileID](242)
	rules, err := NewSelectionRules(false, []SelectionOverride{
		{DirectoryID: directoryHigh, Selected: true},
		{DirectoryID: directoryLow, Selected: true},
		{FileID: fileHigh, Selected: true},
		{FileID: fileLow, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	directories := rules.missingSelectedDirectoryTargets(map[catalog.DirectoryID]struct{}{})
	if len(directories) != 2 || directories[0] != directoryLow || directories[1] != directoryHigh {
		t.Fatalf("missing directory order = %x, want %x then %x", directories, directoryLow.Bytes(), directoryHigh.Bytes())
	}
	files := rules.missingSelectedFileTargets(map[catalog.FileID]struct{}{})
	if len(files) != 2 || files[0] != fileLow || files[1] != fileHigh {
		t.Fatalf("missing file order = %x, want %x then %x", files, fileLow.Bytes(), fileHigh.Bytes())
	}
}

func TestSelectionSnapshotRejectsUnknownAuthorityMode(t *testing.T) {
	rules := SelectionRules{valid: true, mode: SelectionMode(math.MaxUint8)}
	if rules.validSnapshot() {
		t.Fatal("unknown selection authority mode was accepted")
	}
}

func TestSelectionMeasureAndTrackerDefensiveLifecycleEdges(t *testing.T) {
	t.Run("file-count-overflow", func(t *testing.T) {
		measure := SelectionMeasure{DiscoveredFiles: math.MaxUint64}
		measure.addDiscoveredFile(0)
		if !measure.overflowed || measure.DiscoveredFiles != math.MaxUint64 || measure.Class() != SelectionLarge {
			t.Fatalf("overflowed file measure = %+v", measure)
		}
	})

	t.Run("replacement-publishes-exact-snapshot", func(t *testing.T) {
		tracker := newSelectionTracker()
		want := SelectionMeasure{DiscoveredFiles: 7, DiscoveredBytes: 19, DiscoveryTerminalSuccess: true}
		tracker.replace(want)
		if got := tracker.snapshot(); got != want {
			t.Fatalf("replacement snapshot = %+v, want %+v", got, want)
		}
		if got := <-tracker.Updates(); got != want {
			t.Fatalf("replacement update = %+v, want %+v", got, want)
		}
	})

	t.Run("zero-value-close-is-idempotent", func(t *testing.T) {
		var tracker selectionTracker
		tracker.closeUpdates()
		tracker.closeUpdates()
		if !tracker.closed || tracker.Updates() != nil {
			t.Fatalf("zero-value tracker close = closed %t, updates %v", tracker.closed, tracker.Updates())
		}
	})
}
