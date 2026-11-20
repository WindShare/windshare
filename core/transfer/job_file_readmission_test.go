package transfer

import (
	"context"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type scriptedReopenBlocks struct {
	revisions      []content.FileRevisionDescriptor
	reopenOnOffset func(revision content.FileRevision, offset uint64) (nextIndex int, reopen bool)
}

func (reader *scriptedReopenBlocks) ReadRange(
	ctx context.Context,
	_ RevisionHandle,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	if nextIndex, reopen := reader.reopenOnOffset(descriptor.FileRevision(), requested.Offset); reopen {
		return content.NewRevisionReopenedError(descriptor, reader.revisions[nextIndex])
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func metadataRevisionDescriptors(
	t *testing.T,
	share catalog.ShareInstance,
	file catalog.FileID,
	size uint64,
	seeds ...byte,
) []content.FileRevisionDescriptor {
	t.Helper()
	metadata, err := content.SourceMetadataFromBytes(slices.Repeat([]byte{0x7a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	result := make([]content.FileRevisionDescriptor, len(seeds))
	for i, seed := range seeds {
		result[i] = content.BindRevisionMetadata(jobDescriptor(t, share, file, seed, size), metadata)
	}
	return result
}

func TestTransferJobReadmitsAcrossMultipleProgressingReopens(t *testing.T) {
	share := transferID[catalog.ShareInstance](0x81)
	root := transferID[catalog.DirectoryID](0x82)
	file := transferID[catalog.FileID](0x83)
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 3 * window
	descriptors := metadataRevisionDescriptors(t, share, file, size, 1, 2, 3)

	opened, err := NewOpenedRevision(transferID[RevisionHandle](0x84), descriptors[0])
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	var decisions []string
	tracer := TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		if event.Stage == TransferFileRevisionReadmission {
			decisions = append(decisions, event.RevisionReadmission.Decision)
		}
	})
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
			switch {
			case revision == descriptors[0].FileRevision() && offset == window:
				return 1, true
			case revision == descriptors[1].FileRevision() && offset == 2*window:
				return 2, true
			default:
				return 0, false
			}
		},
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "large.bin", size)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: blocks, Materializer: output, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 ||
		result.Progress.VerifiedBytes != size || !result.Progress.CountersExact {
		t.Fatalf("result=%+v decisions=%v", result, decisions)
	}
	wantDecisions := []string{"readmit", "readmitted", "readmit", "readmitted"}
	if !slices.Equal(decisions, wantDecisions) {
		t.Fatalf("decisions=%v, want %v", decisions, wantDecisions)
	}
}

func TestTransferJobStopsAfterConsecutiveStalledReopens(t *testing.T) {
	share := transferID[catalog.ShareInstance](0x91)
	root := transferID[catalog.DirectoryID](0x92)
	file := transferID[catalog.FileID](0x93)
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 3 * window
	descriptors := metadataRevisionDescriptors(t, share, file, size, 11, 12, 13, 14)

	opened, err := NewOpenedRevision(transferID[RevisionHandle](0x94), descriptors[0])
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	var decisions []string
	tracer := TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		if event.Stage == TransferFileRevisionReadmission {
			decisions = append(decisions, event.RevisionReadmission.Decision)
		}
	})
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
			if offset < window {
				return 0, false
			}
			// Attempt 0 persists one window and reopens; attempts 1 and 2 reopen
			// at the same offset without persisting new bytes, hitting retry-limit.
			switch revision {
			case descriptors[0].FileRevision():
				return 1, true
			case descriptors[1].FileRevision():
				return 2, true
			case descriptors[2].FileRevision():
				return 3, true
			default:
				return 0, false
			}
		},
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "stalled.bin", size)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: blocks, Materializer: output, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || len(result.Files) != 1 ||
		result.Files[0].Settlement.Kind() != FilePaused || result.Progress.VerifiedBytes != window {
		t.Fatalf("result=%+v decisions=%v", result, decisions)
	}
	wantDecisions := []string{"readmit", "readmitted", "readmit", "readmitted", "retry-limit"}
	if !slices.Equal(decisions, wantDecisions) {
		t.Fatalf("decisions=%v, want %v", decisions, wantDecisions)
	}
}

func TestTransferJobReconcilesNewlyVerifiedWhenSecondReadmissionBlocked(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xa1)
	root := transferID[catalog.DirectoryID](0xa2)
	blockedFile := transferID[catalog.FileID](0xa3)
	siblingFile := transferID[catalog.FileID](0xa4)
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 3 * window
	siblingSize := uint64(catalog.MinChunkSize)
	blockedDescriptors := metadataRevisionDescriptors(t, share, blockedFile, size, 21, 22, 23)
	siblingDescriptor := jobDescriptor(t, share, siblingFile, 24, siblingSize)

	openedBlocked, err := NewOpenedRevision(transferID[RevisionHandle](0xa5), blockedDescriptors[0])
	if err != nil {
		t.Fatal(err)
	}
	openedSibling, err := NewOpenedRevision(transferID[RevisionHandle](0xa6), siblingDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened: map[catalog.FileID]OpenedRevision{
			blockedFile: openedBlocked,
			siblingFile: openedSibling,
		},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	blockedBegins := 0
	output.beginHook = func(file MaterializationFile) {
		if file.ArtifactPath().String() != "a-blocked.bin" {
			return
		}
		blockedBegins++
		if blockedBegins == 3 {
			// On the 2nd readmission, the staged object disappears so ownership
			// verification withdraws all coverage written across attempts 0 and 1.
			ref, refErr := NewMaterializationStateRef(output.SessionID(), file.Target().Locator().Digest())
			if refErr != nil {
				t.Fatal(refErr)
			}
			settlement, settlementErr := NewImmediateItemBlockedFileSettlement(
				file.Target(), ref, ItemBlockOwnedObjectUnknown,
			)
			if settlementErr != nil {
				t.Fatal(settlementErr)
			}
			output.immediate["a-blocked.bin"] = settlement
		}
	}
	blocks := &scriptedReopenBlocks{
		revisions: blockedDescriptors,
		reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
			switch {
			case revision == blockedDescriptors[0].FileRevision() && offset == window:
				return 1, true
			case revision == blockedDescriptors[1].FileRevision() && offset == 2*window:
				return 2, true
			default:
				return 0, false
			}
		},
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1,
					jobEntry(t, blockedFile, "a-blocked.bin", size),
					jobEntry(t, siblingFile, "b-sibling.bin", siblingSize),
				),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: blocks, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || !result.Progress.CountersExact ||
		result.Progress.VerifiedBytes != siblingSize ||
		result.Progress.NewlyVerifiedBytes != siblingSize ||
		result.Progress.RestartReusableBytes != 0 ||
		result.Progress.FileOutcomes.OwnedObjectUnknownFiles != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobReconcilesCoverageRegressionAcrossMultipleReadmissions(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xb1)
	root := transferID[catalog.DirectoryID](0xb2)
	file := transferID[catalog.FileID](0xb3)
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 3 * window
	descriptors := metadataRevisionDescriptors(t, share, file, size, 31, 32, 33)

	opened, err := NewOpenedRevision(transferID[RevisionHandle](0xb4), descriptors[0])
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	// Seed attempt 0 with a pre-existing recovered range [0, window).
	initialRecovered, err := content.NewRangeSet([]content.Range{{Offset: 0, End: window}})
	if err != nil {
		t.Fatal(err)
	}
	output.durable["regressed.bin"] = initialRecovered
	begins := 0
	output.beginHook = func(file MaterializationFile) {
		begins++
		// Attempt 1 loses all prior coverage (including initialRecovered), then
		// re-downloads [0, window) as newly verified bytes. Attempt 2 loses
		// coverage again before finishing the whole file.
		if begins == 2 || begins == 3 {
			output.durable["regressed.bin"] = content.RangeSet{}
		}
	}
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
			switch {
			case revision == descriptors[0].FileRevision() && offset == 2*window:
				return 1, true
			case revision == descriptors[1].FileRevision() && offset == window:
				return 2, true
			default:
				return 0, false
			}
		},
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "regressed.bin", size)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: blocks, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || !result.Progress.CountersExact ||
		result.Progress.VerifiedBytes != size || result.Progress.NewlyVerifiedBytes != size {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferJobReconcilesPartialCoverageRegressionOnSecondReadmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xc1)
	root := transferID[catalog.DirectoryID](0xc2)
	file := transferID[catalog.FileID](0xc3)
	chunk := uint64(catalog.MinChunkSize)
	window := uint64(defaultFileReadWindowBlocks) * chunk
	size := 3 * window
	descriptors := metadataRevisionDescriptors(t, share, file, size, 41, 42, 43)

	opened, err := NewOpenedRevision(transferID[RevisionHandle](0xc4), descriptors[0])
	if err != nil {
		t.Fatal(err)
	}
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	output := newJobOutput(share)
	regressed, err := content.NewRangeSet([]content.Range{{Offset: 0, End: chunk}})
	if err != nil {
		t.Fatal(err)
	}
	begins := 0
	output.beginHook = func(MaterializationFile) {
		begins++
		// Attempt 1 reuses attempt 0's [0, window) and advances to [0, 2*window).
		// Attempt 2 regresses back inside attempt 0's range to [0, chunk).
		if begins == 3 {
			output.durable["partial-regress.bin"] = regressed
		}
	}
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
			switch {
			case revision == descriptors[0].FileRevision() && offset == window:
				return 1, true
			case revision == descriptors[1].FileRevision() && offset == 2*window:
				return 2, true
			default:
				return 0, false
			}
		},
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "partial-regress.bin", size)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: blocks, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || !result.Progress.CountersExact ||
		result.Progress.VerifiedBytes != size || result.Progress.NewlyVerifiedBytes != size {
		t.Fatalf("result=%+v", result)
	}
}

func TestReadmissionRangeSetOperations(t *testing.T) {
	mustRanges := func(ranges ...content.Range) content.RangeSet {
		t.Helper()
		set, err := content.NewRangeSet(ranges)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	left := mustRanges(
		content.Range{Offset: 0, End: 10},
		content.Range{Offset: 12, End: 25},
	)
	right := mustRanges(
		content.Range{Offset: 2, End: 4},
		content.Range{Offset: 8, End: 15},
		content.Range{Offset: 20, End: 25},
	)
	wantIntersect := []content.Range{
		{Offset: 2, End: 4},
		{Offset: 8, End: 10},
		{Offset: 12, End: 15},
		{Offset: 20, End: 25},
	}
	if got := intersectRanges(left, right).Ranges(); !slices.Equal(got, wantIntersect) {
		t.Fatalf("intersectRanges=%v, want %v", got, wantIntersect)
	}
	wantSubtract := []content.Range{
		{Offset: 0, End: 2},
		{Offset: 4, End: 8},
		{Offset: 15, End: 20},
	}
	if got := subtractRanges(left, right).Ranges(); !slices.Equal(got, wantSubtract) {
		t.Fatalf("subtractRanges=%v, want %v", got, wantSubtract)
	}
	if got := subtractRanges(left, content.RangeSet{}).Ranges(); !slices.Equal(got, left.Ranges()) {
		t.Fatalf("subtractRanges(left, empty)=%v, want %v", got, left.Ranges())
	}
	if !intersectRanges(left, content.RangeSet{}).IsEmpty() {
		t.Fatalf("intersectRanges(left, empty) should be empty")
	}
}
