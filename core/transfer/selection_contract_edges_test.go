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

func TestSelectionDecisionPreservesTheRuleThatAdmittedAFile(t *testing.T) {
	file := transferID[catalog.FileID](244)
	inherited, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := NewPathSelectionRules([]string{"nested/file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		rules SelectionRules
		path  string
		want  FileSelectionDecision
	}{
		{name: "inherited", rules: inherited, want: FileSelectionInherited},
		{name: "node override", rules: node, want: FileSelectionNodeOverride},
		{name: "catalog path", rules: path, path: "nested/file.bin", want: FileSelectionCatalogPathTarget},
	} {
		if got := test.rules.selectedFileDecision(file, test.path); got != test.want {
			t.Errorf("%s decision=%d want=%d", test.name, got, test.want)
		}
	}
}

func TestSelectionMeasureAndTrackerDefensiveLifecycleEdges(t *testing.T) {
	t.Run("discovery-and-completion-counters", func(t *testing.T) {
		tracker := newSelectionTracker()
		initial := <-tracker.Updates()
		if initial.Discovery != DiscoveryOpen || initial.DiscoveredFiles != 0 || initial.CompletedFiles != 0 {
			t.Fatalf("initial measure = %+v", initial)
		}
		tracker.addFile(5)
		tracker.completeFile(5)
		open := tracker.snapshot()
		if open.Discovery != DiscoveryOpen || open.DiscoveredFiles != 1 || open.DiscoveredBytes != 5 ||
			open.CompletedFiles != 1 || open.CompletedBytes != 5 || open.ConnectionSizeClass() != ConnectionSizeUnknown {
			t.Fatalf("open measure = %+v", open)
		}
		tracker.finishDiscovery()
		complete := tracker.snapshot()
		if complete.Discovery != DiscoveryComplete || !complete.DiscoveryTerminalSuccess || complete.ConnectionSizeClass() != ConnectionSizeSmall {
			t.Fatalf("complete measure = %+v", complete)
		}
		// Discovery status is terminal; a late failure report from another
		// settlement path must not rewrite a completed catalog into failed.
		tracker.failDiscovery()
		if got := tracker.snapshot(); got.Discovery != DiscoveryComplete || got.DiscoveryTerminalSuccess != true {
			t.Fatalf("late failure regressed discovery status: %+v", got)
		}
	})

	t.Run("file-count-overflow", func(t *testing.T) {
		measure := SelectionMeasure{DiscoveredFiles: math.MaxUint64}
		measure.addDiscoveredFile(0)
		if !measure.overflowed || measure.DiscoveredFiles != math.MaxUint64 || measure.ConnectionSizeClass() != ConnectionSizeLarge {
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
