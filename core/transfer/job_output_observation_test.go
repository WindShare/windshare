package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type outputObservedRangeReader struct {
	buffered func()
	accepted func(uint64)
}

func (r outputObservedRangeReader) ReadRange(ctx context.Context, _ RevisionHandle, _ content.FileRevisionDescriptor, requested content.Range, sink RangeSink) error {
	if err := sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length())); err != nil {
		return err
	}
	r.buffered()
	return nil
}
func (r outputObservedRangeReader) RecordOutput(bytes uint64) { r.accepted(bytes) }

func TestOutputObservationWaitsForMaterializerAcceptance(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "failed"}[fails], func(t *testing.T) {
			var accepted uint64
			var output *jobOutput
			reader := outputObservedRangeReader{
				buffered: func() {
					if accepted != 0 || !output.transactions["file.bin"].pending.IsEmpty() {
						t.Error("buffered authentication was counted as materialized output")
					}
				},
				accepted: func(bytes uint64) {
					if output.transactions["file.bin"].pending.IsEmpty() {
						t.Error("output observed before its transaction accepted bytes")
					}
					accepted += bytes
				},
			}
			var job *TransferJob
			job, output = newAtomicRangeSinkJob(t, reader)
			if fails {
				output.transactionScript.writeErr = errors.New("output unavailable")
			}
			result := job.Run(t.Context())
			if fails {
				if accepted != 0 || result.Outcome == DirectTreeOutcomeSuccess {
					t.Fatalf("failed output advanced progress: bytes=%d result=%+v", accepted, result)
				}
			} else if accepted != 2*uint64(catalog.MinChunkSize) || result.Outcome != DirectTreeOutcomeSuccess {
				t.Fatalf("accepted bytes=%d result=%+v", accepted, result)
			}
		})
	}
}
