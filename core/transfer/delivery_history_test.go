package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type historicalJobOutput struct {
	*jobOutput
	receipt HistoricalDelivery
}

func (output *historicalJobOutput) OpenDirectTree(ctx context.Context, intent ReceiveIntent) (DirectTreeSession, error) {
	_, err := output.jobOutput.OpenDirectTree(ctx, intent)
	output.receipt.OperationID = intent.OperationID()
	return output, err
}
func (output *historicalJobOutput) LookupDelivery(_ context.Context, file catalog.FileID, path string) (HistoricalDelivery, bool, error) {
	return output.receipt, output.receipt.FileID == file && output.receipt.MaterializationPath == path, nil
}

func TestDeliveryReceiptSkipsSourceAcquisitionDespiteRevisionDrift(t *testing.T) {
	share, root, file := transferID[catalog.ShareInstance](1), transferID[catalog.DirectoryID](2), transferID[catalog.FileID](3)
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed source", true: "missing source"}[unavailable], func(t *testing.T) {
			output := &historicalJobOutput{jobOutput: newJobOutput(share), receipt: HistoricalDelivery{
				FileID: file, FileRevision: transferID[content.FileRevision](4), MaterializationPath: "done.bin", Size: 4,
			}}
			revisions := &jobRevisionClient{opened: map[catalog.FileID]OpenedRevision{}, failures: map[catalog.FileID]error{}}
			if unavailable {
				revisions.failures[file] = errors.New("source disappeared")
			} else {
				revisions.opened[file], _ = NewOpenedRevision(transferID[RevisionHandle](8), jobDescriptor(t, share, file, 9, 8))
			}
			snapshot := jobSnapshot(t, share, root, 6, jobEntry(t, file, "done.bin", 8))
			client, _ := newJobCatalogClient(t, share, snapshot)
			rules, _ := NewSelectionRules(true, nil)
			var skipped bool
			job, err := newTestTransferJob(t, testTransferJobConfig{ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog: client, Revisions: revisions, Blocks: scriptedRangeReader{err: errors.New("must not redownload")}, Materializer: output,
				Tracer: TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
					if event.Stage == TransferDeliverySkipped && event.FileID == file {
						skipped = true
					}
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(context.Background())
			p := result.Progress
			if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || len(revisions.order) != 0 || len(output.transactions) != 0 || !skipped {
				t.Fatalf("receipt reopened the source or output: %+v, opened=%v", result, revisions.order)
			}
			if p.PreviouslyPublishedBytes != 4 || p.DiscoveredBytes != 4 || p.VerifiedBytes != 0 || p.NewlyVerifiedBytes != 0 || p.RestartReusableBytes != 0 || !p.CountersExact {
				t.Fatalf("historical delivery was counted as reusable/current bytes: %+v", p)
			}
		})
	}
}

func TestAcceptedAndPersistedRangesHaveIndependentGenerations(t *testing.T) {
	binding, _ := outputLifecycleFixture(t)
	empty, _ := content.NewRangeSet(nil)
	half, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: binding.ExactSize() / 2}})
	full, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: binding.ExactSize()}})
	accepted, _ := VerifyProcessRanges(binding, 10, full)
	persisted, _ := VerifyRestartRanges(binding, 1, half)
	progress, err := NewFileProgress(accepted, persisted)
	if err != nil {
		t.Fatal(err)
	}
	fileProgress := fileTransferProgress{persisted: persisted, persistedBytes: binding.ExactSize() / 2}
	newer, _ := VerifyRestartRanges(binding, 2, full)
	progress, err = NewFileProgress(accepted, newer)
	if err != nil {
		t.Fatal(err)
	}
	delta, ok := fileProgress.observePersistence(progress)
	if !ok || delta != binding.ExactSize()/2 {
		t.Fatalf("independent generations rejected: %d %v", delta, ok)
	}
	stale, _ := NewFileProgress(accepted, persisted)
	if _, ok := fileProgress.observePersistence(stale); ok {
		t.Fatal("persisted progress regressed")
	}
	narrower, _ := VerifyProcessRanges(binding, 11, empty)
	if _, err := NewFileProgress(narrower, newer); err == nil {
		t.Fatal("persisted ranges exceeded accepted coverage")
	}
}
