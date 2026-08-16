package transfer

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
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
	t.Run("discovery-and-verified-counters", func(t *testing.T) {
		tracker := newReceiveProgressTracker()
		initial := <-tracker.Updates()
		if initial.Discovery != DiscoveryOpen || initial.DiscoveredFiles != 0 || !initial.CountersExact {
			t.Fatalf("initial progress = %+v", initial)
		}
		selection := newDiscoveredSelection()
		selection.addFile(5)
		tracker.addDiscovery(selection)
		tracker.addNewlyVerified(5)
		open := tracker.snapshotValue()
		if open.Discovery != DiscoveryOpen || open.DiscoveredFiles != 1 || open.DiscoveredBytes != 5 ||
			open.VerifiedBytes != 5 || open.NewlyVerifiedBytes != 5 || open.ConnectionSizeClass() != ConnectionSizeUnknown {
			t.Fatalf("open progress = %+v", open)
		}
		tracker.finishDiscovery()
		complete := tracker.snapshotValue()
		if complete.Discovery != DiscoveryComplete || complete.ConnectionSizeClass() != ConnectionSizeSmall {
			t.Fatalf("complete progress = %+v", complete)
		}
		// Discovery status is terminal; a late failure report from another
		// settlement path must not rewrite a completed catalog into failed.
		tracker.failDiscovery()
		if got := tracker.snapshotValue(); got.Discovery != DiscoveryComplete {
			t.Fatalf("late failure regressed discovery status: %+v", got)
		}
	})

	t.Run("file-count-overflow", func(t *testing.T) {
		tracker := newReceiveProgressTracker()
		tracker.addDiscovery(discoveredSelection{files: math.MaxUint64, exact: true})
		tracker.addDiscovery(discoveredSelection{files: 1, exact: true})
		progress := tracker.snapshotValue()
		if progress.CountersExact || progress.DiscoveredFiles != math.MaxUint64 ||
			progress.ConnectionSizeClass() != ConnectionSizeLarge {
			t.Fatalf("overflowed progress = %+v", progress)
		}
	})

	t.Run("latest-update-coalesces", func(t *testing.T) {
		tracker := newReceiveProgressTracker()
		tracker.addDiscovery(discoveredSelection{files: 3, bytes: 7, exact: true})
		tracker.addDiscovery(discoveredSelection{files: 4, bytes: 12, exact: true})
		want := tracker.snapshotValue()
		if got := <-tracker.Updates(); got != want {
			t.Fatalf("coalesced update = %+v, want %+v", got, want)
		}
	})

	t.Run("zero-value-close-is-idempotent", func(t *testing.T) {
		var tracker receiveProgressTracker
		tracker.closeUpdates()
		tracker.closeUpdates()
		if !tracker.closed || tracker.Updates() != nil {
			t.Fatalf("zero-value tracker close = closed %t, updates %v", tracker.closed, tracker.Updates())
		}
	})
}

func TestReceiveProgressSnapshotRemainsExactDuringConcurrentDiscoveryAndVerification(t *testing.T) {
	const workers = 64
	tracker := newReceiveProgressTracker()
	start := make(chan struct{})
	stop := make(chan struct{})
	invalid := make(chan ReceiveProgressSnapshot, 1)
	var observers sync.WaitGroup
	observers.Go(func() {
		previous := tracker.snapshotValue()
		for {
			current := tracker.snapshotValue()
			if current.DiscoveredFiles < previous.DiscoveredFiles ||
				current.DiscoveredBytes < previous.DiscoveredBytes ||
				current.VerifiedBytes < previous.VerifiedBytes ||
				current.NewlyVerifiedBytes < previous.NewlyVerifiedBytes ||
				!current.CountersExact || current.NewlyVerifiedBytes > current.VerifiedBytes ||
				current.VerifiedBytes > current.DiscoveredBytes {
				select {
				case invalid <- current:
				default:
				}
				return
			}
			previous = current
			select {
			case <-stop:
				return
			default:
			}
		}
	})

	var mutations sync.WaitGroup
	for range workers {
		mutations.Go(func() {
			<-start
			tracker.addDiscovery(discoveredSelection{files: 1, bytes: 1, exact: true})
			tracker.addNewlyVerified(1)
		})
	}
	close(start)
	mutations.Wait()
	tracker.finishDiscovery()
	close(stop)
	observers.Wait()
	select {
	case snapshot := <-invalid:
		t.Fatalf("invalid concurrent snapshot: %+v", snapshot)
	default:
	}
	final := tracker.snapshotValue()
	if final.Discovery != DiscoveryComplete || final.DiscoveredFiles != workers ||
		final.DiscoveredBytes != workers || final.VerifiedBytes != workers ||
		final.NewlyVerifiedBytes != workers || !final.CountersExact {
		t.Fatalf("final concurrent snapshot: %+v", final)
	}
}

func TestReceiveProgressCounterSaturationIsAbsorbing(t *testing.T) {
	_, checkpoint := outputLifecycleFixture(t)
	paused, err := NewVerifiedFileSettlement(FilePaused, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	tracker := newReceiveProgressTracker()
	tracker.snapshot.FileOutcomes.PausedFiles = math.MaxUint64
	tracker.acceptFileSettlement(paused, 0)
	tracker.addDiscovery(discoveredSelection{files: 1, bytes: 1, exact: true})
	progress := tracker.snapshotValue()
	if progress.FileOutcomes.PausedFiles != math.MaxUint64 || progress.CountersExact ||
		progress.DiscoveredFiles != 1 ||
		(FileOutcomeSummary{DownloadedFiles: math.MaxUint64, ResumedFiles: 1}).PublishedFiles() != math.MaxUint64 {
		t.Fatalf("saturation was not absorbing: %+v", progress)
	}
}
