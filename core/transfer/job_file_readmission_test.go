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
