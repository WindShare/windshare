package transfer

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type sequencedRevisionClient struct {
	mu          sync.Mutex
	revisions   map[catalog.FileID][]content.FileRevisionDescriptor
	opens       map[catalog.FileID]int
	active      map[RevisionHandle]content.FileRevisionDescriptor
	openedCount int
	released    int
	nextSeed    byte
}

func newSequencedRevisionClient(revisions map[catalog.FileID][]content.FileRevisionDescriptor) *sequencedRevisionClient {
	return &sequencedRevisionClient{
		revisions: revisions,
		opens:     make(map[catalog.FileID]int),
		active:    make(map[RevisionHandle]content.FileRevisionDescriptor),
		nextSeed:  1,
	}
}

func (c *sequencedRevisionClient) OpenRevision(_ context.Context, file catalog.FileID) (OpenedRevision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.revisions[file]
	if len(list) == 0 {
		return OpenedRevision{}, content.ErrRevisionNotFound
	}
	idx := c.opens[file]
	if idx >= len(list) {
		idx = len(list) - 1
	}
	c.opens[file]++
	handle := transferID[RevisionHandle](c.nextSeed)
	c.nextSeed++
	c.active[handle] = list[idx]
	c.openedCount++
	return NewOpenedRevision(handle, list[idx])
}

func (c *sequencedRevisionClient) ReleaseRevision(_ context.Context, handle RevisionHandle) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.active[handle]; !ok {
		return content.ErrInvalidLease
	}
	delete(c.active, handle)
	c.released++
	return nil
}

func (c *sequencedRevisionClient) assertAllReleased(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.active) != 0 || c.released != c.openedCount {
		t.Fatalf("handle leak: opened=%d released=%d active=%d", c.openedCount, c.released, len(c.active))
	}
}

type scriptedReopenBlocks struct {
	revisions      []content.FileRevisionDescriptor
	client         *sequencedRevisionClient
	reopenOnOffset func(revision content.FileRevision, offset uint64) (nextIndex int, reopen bool)
}

func (reader *scriptedReopenBlocks) ReadRange(
	ctx context.Context,
	handle RevisionHandle,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	if reader.client != nil {
		reader.client.mu.Lock()
		bound, ok := reader.client.active[handle]
		reader.client.mu.Unlock()
		if !ok || bound != descriptor {
			return ErrBlockIdentity
		}
	}
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

	revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
		file: descriptors,
	})
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	stream := newTestDiagnosticsStream(t)
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		client:    revisions,
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
		Revisions: revisions, Blocks: blocks, Materializer: output, Diagnostics: stream.Sink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	revisions.assertAllReleased(t)
	var decisions []string
	for _, rec := range drainTestDiagnostics(stream) {
		if rec.Payload().Get("stage").Text() == TransferFileRevisionReadmission.DiagnosticName() {
			decisions = append(decisions, rec.Payload().Get("revision_readmission").Get("decision").Text())
		}
	}
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

	revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
		file: descriptors,
	})
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	stream := newTestDiagnosticsStream(t)
	blocks := &scriptedReopenBlocks{
		revisions: descriptors,
		client:    revisions,
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
		Revisions: revisions, Blocks: blocks, Materializer: output, Diagnostics: stream.Sink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	revisions.assertAllReleased(t)
	var decisions []string
	for _, rec := range drainTestDiagnostics(stream) {
		if rec.Payload().Get("stage").Text() == TransferFileRevisionReadmission.DiagnosticName() {
			decisions = append(decisions, rec.Payload().Get("revision_readmission").Get("decision").Text())
		}
	}
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

	revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
		blockedFile: blockedDescriptors,
		siblingFile: {siblingDescriptor},
	})
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
		client:    revisions,
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
	revisions.assertAllReleased(t)
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

	revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
		file: descriptors,
	})
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
		client:    revisions,
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
	revisions.assertAllReleased(t)
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

	revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
		file: descriptors,
	})
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
		client:    revisions,
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
	revisions.assertAllReleased(t)
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

func TestTransferJobReconcilesReadmissionImmediateSettlements(t *testing.T) {
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 3 * window
	siblingSize := uint64(catalog.MinChunkSize)
	objectID := func() OwnedObjectID {
		var id OwnedObjectID
		for i := range id {
			id[i] = byte(i + 1)
		}
		return id
	}()
	itemBlocked := func(reason ItemBlockReason) func(*testing.T, *jobOutput, MaterializationFile) FileSettlement {
		return func(t *testing.T, output *jobOutput, file MaterializationFile) FileSettlement {
			t.Helper()
			ref, err := NewMaterializationStateRef(output.SessionID(), file.Target().Locator().Digest())
			if err != nil {
				t.Fatal(err)
			}
			settlement, err := NewImmediateItemBlockedFileSettlement(file.Target(), ref, reason)
			if err != nil {
				t.Fatal(err)
			}
			return settlement
		}
	}
	boundSettlement := func(build func(MaterializedFileBinding) (FileSettlement, error)) func(*testing.T, *jobOutput, MaterializationFile) FileSettlement {
		return func(t *testing.T, _ *jobOutput, file MaterializationFile) FileSettlement {
			t.Helper()
			binding, err := BindFileMaterializationTarget(file.Target(), objectID)
			if err != nil {
				t.Fatal(err)
			}
			settlement, err := build(binding)
			if err != nil {
				t.Fatal(err)
			}
			return settlement
		}
	}

	for _, test := range []struct {
		name              string
		settlement        func(*testing.T, *jobOutput, MaterializationFile) FileSettlement
		wantOutcome       DirectTreeOutcome
		wantVerified      uint64
		wantNewlyVerified uint64
		wantReusable      uint64
		wantPrevPublished uint64
		wantOutcomes      FileOutcomeSummary
	}{
		{
			name:              "checkpoint invalid",
			settlement:        itemBlocked(ItemBlockCheckpointInvalid),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1, CheckpointInvalidFiles: 1},
		},
		{
			name:              "revision conflict",
			settlement:        itemBlocked(ItemBlockRevisionConflict),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1, RevisionConflictFiles: 1},
		},
		{
			name:              "state corrupt",
			settlement:        itemBlocked(ItemBlockStateCorrupt),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1},
		},
		{
			name:              "publication ambiguous",
			settlement:        itemBlocked(ItemBlockPublicationAmbiguous),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1},
		},
		{
			name:              "retirement uncertain",
			settlement:        itemBlocked(ItemBlockRetirementUncertain),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, ItemBlockedFiles: 1},
		},
		{
			name:              "retired file failed",
			settlement:        boundSettlement(NewFailedFileSettlement),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, FailedFiles: 1},
		},
		{
			name:              "previously published",
			settlement:        boundSettlement(NewPreviouslyPublishedFileSettlement),
			wantOutcome:       DirectTreeOutcomeSuccess,
			wantVerified:      siblingSize,
			wantNewlyVerified: siblingSize,
			wantPrevPublished: size,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, PreviouslyPublishedFiles: 1},
		},
		{
			name:              "reversible collision preserves partial coverage",
			settlement:        boundSettlement(NewTransactionCollisionFileSettlement),
			wantOutcome:       DirectTreeOutcomePartial,
			wantVerified:      2*window + siblingSize,
			wantNewlyVerified: 2*window + siblingSize,
			wantReusable:      2 * window,
			wantOutcomes:      FileOutcomeSummary{DownloadedFiles: 1, CollisionFiles: 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](0xd1)
			root := transferID[catalog.DirectoryID](0xd2)
			targetFile := transferID[catalog.FileID](0xd3)
			siblingFile := transferID[catalog.FileID](0xd4)
			descriptors := metadataRevisionDescriptors(t, share, targetFile, size, 51, 52, 53)
			siblingDescriptor := jobDescriptor(t, share, siblingFile, 54, siblingSize)

			revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
				targetFile:  descriptors,
				siblingFile: {siblingDescriptor},
			})
			output := newJobOutput(share)
			begins := 0
			output.beginHook = func(file MaterializationFile) {
				if file.ArtifactPath().String() != "a-target.bin" {
					return
				}
				begins++
				if begins == 3 {
					output.immediate["a-target.bin"] = test.settlement(t, output, file)
				}
			}
			blocks := &scriptedReopenBlocks{
				revisions: descriptors,
				client:    revisions,
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
						root: jobSnapshot(t, share, root, 1,
							jobEntry(t, targetFile, "a-target.bin", size),
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
			revisions.assertAllReleased(t)
			progress := result.Progress
			if result.Outcome != test.wantOutcome || !progress.CountersExact ||
				progress.VerifiedBytes != test.wantVerified ||
				progress.NewlyVerifiedBytes != test.wantNewlyVerified ||
				progress.RestartReusableBytes != test.wantReusable ||
				progress.PreviouslyPublishedBytes != test.wantPrevPublished ||
				progress.FileOutcomes != test.wantOutcomes {
				t.Fatalf("result=%+v progress=%+v", result, progress)
			}
		})
	}
}

func TestTransferJobRejectsReadmissionWhenFreshOpenChangesOrLosesMetadata(t *testing.T) {
	window := uint64(defaultFileReadWindowBlocks) * uint64(catalog.MinChunkSize)
	size := 2 * window

	for _, tc := range []struct {
		name     string
		metadata []byte
		wantErr  error
	}{
		{"metadata changed", slices.Repeat([]byte{0x99}, 32), content.ErrRevisionStale},
		{"metadata unavailable", nil, content.ErrRevisionReopened},
	} {
		t.Run(tc.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](0xe1)
			root := transferID[catalog.DirectoryID](0xe2)
			file := transferID[catalog.FileID](0xe3)
			initialPair := metadataRevisionDescriptors(t, share, file, size, 71, 72)
			fresh := jobDescriptor(t, share, file, 73, size)
			if tc.metadata != nil {
				meta, err := content.SourceMetadataFromBytes(tc.metadata)
				if err != nil {
					t.Fatal(err)
				}
				fresh = content.BindRevisionMetadata(fresh, meta)
			}
			revisions := newSequencedRevisionClient(map[catalog.FileID][]content.FileRevisionDescriptor{
				file: {initialPair[0], fresh},
			})
			output := newJobOutput(share)
			blocks := &scriptedReopenBlocks{
				revisions: initialPair,
				client:    revisions,
				reopenOnOffset: func(revision content.FileRevision, offset uint64) (int, bool) {
					return 1, revision == initialPair[0].FileRevision() && offset == window
				},
			}
			rules, _ := NewSelectionRules(true, nil)
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: failingCatalog{
					snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
						root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "changed.bin", size)),
					},
					failures: make(map[catalog.DirectoryID]error),
				},
				Revisions: revisions, Blocks: blocks, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(context.Background())
			revisions.assertAllReleased(t)
			if result.Outcome != DirectTreeOutcomePartial || len(result.Files) != 1 ||
				result.Files[0].Settlement.Kind() != FilePaused ||
				!errors.Is(result.Files[0].Cause, tc.wantErr) ||
				!result.Progress.CountersExact || result.Progress.VerifiedBytes != window {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}
