package transfer

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type adversarialRangeReaderMode uint8

const (
	adversarialRangeExtraFuture adversarialRangeReaderMode = iota + 1
	adversarialRangeOutside
	adversarialRangeDuplicate
	adversarialRangePartial
	adversarialRangeErrorAfterWrite
	adversarialRangeSwallowedViolation
)

type adversarialRangeReader struct {
	mode adversarialRangeReaderMode
	err  error
}

func (reader adversarialRangeReader) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	length := int(requested.Length())
	data := make([]byte, length)
	switch reader.mode {
	case adversarialRangeExtraFuture:
		if err := sink.WriteRange(ctx, requested.Offset, data); err != nil {
			return err
		}
		return sink.WriteRange(ctx, requested.End, []byte{1})
	case adversarialRangeOutside:
		return sink.WriteRange(ctx, requested.End, []byte{1})
	case adversarialRangeDuplicate:
		half := max(1, length/2)
		if err := sink.WriteRange(ctx, requested.Offset, data[:half]); err != nil {
			return err
		}
		return sink.WriteRange(ctx, requested.Offset, data[:half])
	case adversarialRangePartial:
		return sink.WriteRange(ctx, requested.Offset, data[:max(1, length/2)])
	case adversarialRangeErrorAfterWrite:
		if err := sink.WriteRange(ctx, requested.Offset, data); err != nil {
			return err
		}
		return reader.err
	case adversarialRangeSwallowedViolation:
		if err := sink.WriteRange(ctx, requested.Offset, data); err != nil {
			return err
		}
		_ = sink.WriteRange(ctx, requested.End, []byte{1})
		return reader.err
	default:
		return errors.New("unsupported adversarial range-reader mode")
	}
}

func TestTransferJobAtomicallyRejectsMalformedRangeReaderOutput(t *testing.T) {
	readerFailure := errors.New("range source disconnected after write")
	for _, test := range []struct {
		name         string
		reader       adversarialRangeReader
		wantContract bool
		wantCause    error
	}{
		{name: "extra future range", reader: adversarialRangeReader{mode: adversarialRangeExtraFuture}, wantContract: true},
		{name: "outside requested range", reader: adversarialRangeReader{mode: adversarialRangeOutside}, wantContract: true},
		{name: "duplicate bytes", reader: adversarialRangeReader{mode: adversarialRangeDuplicate}, wantContract: true},
		{name: "partial nil success", reader: adversarialRangeReader{mode: adversarialRangePartial}, wantContract: true},
		{
			name:      "reader error after complete buffered write",
			reader:    adversarialRangeReader{mode: adversarialRangeErrorAfterWrite, err: readerFailure},
			wantCause: readerFailure,
		},
		{
			name:         "reader swallows sink violation",
			reader:       adversarialRangeReader{mode: adversarialRangeSwallowedViolation, err: readerFailure},
			wantContract: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, output := newAtomicRangeSinkJob(t, test.reader)
			result := job.Run(context.Background())
			if result.Outcome != DirectTreeOutcomeResumable || result.TerminationCause == nil ||
				result.Settlement.Kind() != DirectTreeSettlementResumable || result.SettlementFailure != nil {
				t.Fatalf("atomic range result = %+v", result)
			}
			if test.wantContract {
				if normalizedFault(result.TerminationCause) != fault.DependencyContractFault() {
					t.Fatalf("termination cause = %v, want range-reader dependency contract", result.TerminationCause)
				}
			} else if normalizedFault(result.TerminationCause) != fault.DependencyContractFault() {
				t.Fatalf("termination cause = %v, want unknown reader error to fail closed", result.TerminationCause)
			}
			transaction := output.transactions["file.bin"]
			if transaction == nil || !transaction.pending.IsEmpty() || !transaction.durable.IsEmpty() ||
				transaction.commitCalls != 0 || len(transaction.pauseReasons) != 1 || output.pauseCalls != 1 {
				t.Fatalf("malformed reader touched output: transaction=%+v pauseCalls=%d", transaction, output.pauseCalls)
			}
			if len(result.Files) != 1 || result.Files[0].Stage != FailureBlockTransfer ||
				result.Files[0].Settlement.Kind() != FilePaused {
				t.Fatalf("file failure = %+v", result.Files)
			}
		})
	}
}

func TestAtomicRequestedRangeSinkEnforcesAllocationAndArithmeticBounds(t *testing.T) {
	target := RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil })
	for _, requested := range []content.Range{
		{},
		{Offset: 2, End: 1},
		{Offset: 0, End: uint64(catalog.MaxChunkSize) + 1},
	} {
		if _, err := newAtomicRequestedRangeSink(requested, target); !isJobTerminalError(err) || normalizedFault(err) != fault.DependencyContractFault() {
			t.Fatalf("request %v error = %v, want bounded dependency contract", requested, err)
		}
	}
	maximum, err := newAtomicRequestedRangeSink(
		content.Range{Offset: math.MaxUint64 - uint64(catalog.MaxChunkSize), End: math.MaxUint64},
		target,
	)
	if err != nil || len(maximum.data) != catalog.MaxChunkSize {
		t.Fatalf("maximum atomic request = (bytes=%d, err=%v)", len(maximum.data), err)
	}
	if err := maximum.WriteRange(context.Background(), math.MaxUint64, []byte{1}); !isJobTerminalError(err) || normalizedFault(err) != fault.DependencyContractFault() {
		t.Fatalf("overflowing write error = %v, want dependency contract", err)
	}
}

func newAtomicRangeSinkJob(t *testing.T, reader RangeReader) (*TransferJob, *jobOutput) {
	t.Helper()
	share := transferID[catalog.ShareInstance](200)
	root := transferID[catalog.DirectoryID](201)
	file := transferID[catalog.FileID](202)
	exactSize := 2 * uint64(catalog.MinChunkSize)
	descriptor := jobDescriptor(t, share, file, 203, exactSize)
	opened, err := NewOpenedRevision(transferID[content.LeaseID](204), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share,
		SyntheticRoot: root,
		Rules:         rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 205, jobEntry(t, file, "file.bin", exactSize)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks:       reader,
		Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job, output
}
