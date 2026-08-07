package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type rangeBlockResult struct {
	index uint64
	data  []byte
	err   error
}

type RangeSink interface {
	WriteRange(context.Context, uint64, []byte) error
}

type RangeSinkFunc func(context.Context, uint64, []byte) error

func (function RangeSinkFunc) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	return function(ctx, offset, data)
}

// ReadRange requests only the file-local blocks intersecting requested. The
// sink sees exact requested bytes; first/last block over-read never escapes the
// broker and is strictly less than two chunk lengths in total.
func (b *BlockBroker) ReadRange(
	ctx context.Context,
	leaseID content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	if sink == nil {
		return errors.New("range read requires a sink")
	}
	if _, err := content.NewRangeSet([]content.Range{requested}); err != nil || requested.End > descriptor.ExactSize() {
		return errors.Join(ErrInvalidDemand, err)
	}
	if _, err := validateDemand(b.share, leaseID, descriptor, requested.Offset/uint64(descriptor.Geometry().ChunkSize())); err != nil {
		return err
	}
	chunkSize := uint64(descriptor.Geometry().ChunkSize())
	first := requested.Offset / chunkSize
	last := (requested.End - 1) / chunkSize
	readContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan rangeBlockResult, b.maxConcurrentBlocks)
	launch := func(index uint64) {
		go func() {
			data, err := b.GetBlock(readContext, leaseID, descriptor, index)
			results <- rangeBlockResult{index: index, data: data, err: err}
		}()
	}
	nextLaunch := first
	nextWrite := first
	inflight := 0
	for inflight < b.maxConcurrentBlocks && nextLaunch <= last {
		launch(nextLaunch)
		nextLaunch++
		inflight++
	}
	pending := make(map[uint64][]byte, b.maxConcurrentBlocks)
	for nextWrite <= last {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			inflight--
			if result.err != nil {
				return fmt.Errorf("read file-local block %d: %w", result.index, result.err)
			}
			pending[result.index] = result.data
		}
		for {
			data, ready := pending[nextWrite]
			if !ready {
				break
			}
			blockOffset := nextWrite * chunkSize
			start := max(requested.Offset, blockOffset) - blockOffset
			end := min(requested.End, blockOffset+uint64(len(data))) - blockOffset
			if start >= end || end > uint64(len(data)) {
				return ErrBlockIdentity
			}
			if err := sink.WriteRange(ctx, blockOffset+start, data[start:end]); err != nil {
				return err
			}
			delete(pending, nextWrite)
			nextWrite++
			if nextLaunch <= last {
				launch(nextLaunch)
				nextLaunch++
				inflight++
			}
		}
		if inflight == 0 && nextWrite <= last {
			return ErrBlockIdentity
		}
	}
	return nil
}

func (r *jobRun) transferMissingRanges(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	checkpoint VerifiedDurableRanges,
) (bool, error) {
	durableOutput := r.output.Capabilities().Durability != DurabilityNone
	if !durableOutput && !checkpoint.Ranges().IsEmpty() {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0,
		)
	}
	missing, err := MissingRanges(opened.Descriptor.ExactSize(), checkpoint.Ranges())
	if err != nil {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(err), 0,
		)
	}
	wrote, transientEnd := false, uint64(0)
	chunk := uint64(opened.Descriptor.Geometry().ChunkSize())
	for _, current := range missing.Ranges() {
		for offset := current.Offset; offset < current.End; {
			if cause := context.Cause(ctx); cause != nil {
				return false, r.settleFailedFile(
					ctx, plan, opened, transaction, FailureBlockTransfer, cause, 0,
				)
			}
			next := min(current.End, ((offset/chunk)+1)*chunk)
			requested := content.Range{Offset: offset, End: next}
			if !durableOutput && requested.Offset != transientEnd {
				return false, r.settleFailedFile(
					ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0,
				)
			}
			advanced, transferred, transferErr := r.transferRequestedRange(
				ctx, plan, opened, transaction, checkpoint, requested, durableOutput,
			)
			if transferErr != nil || !transferred {
				return false, transferErr
			}
			checkpoint = advanced
			transientEnd = requested.End
			if !wrote {
				r.traceFileLifecycle(TransferFileFirstWrite, plan, false)
				wrote = true
			}
			offset = next
		}
	}
	complete := RangesCoverFile(opened.Descriptor.ExactSize(), checkpoint.Ranges())
	if !durableOutput {
		complete = transientEnd == opened.Descriptor.ExactSize()
	}
	if !complete {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0,
		)
	}
	return true, nil
}

func (r *jobRun) transferRequestedRange(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	prior VerifiedDurableRanges,
	requested content.Range,
	durableOutput bool,
) (VerifiedDurableRanges, bool, error) {
	buffered, err := newAtomicRequestedRangeSink(requested, transaction)
	if err == nil {
		err = r.job.blocks.ReadRange(ctx, opened.LeaseID, opened.Descriptor, requested, buffered)
	}
	if bufferedErr := buffered.Failure(); bufferedErr != nil {
		err = bufferedErr
	}
	if err != nil {
		inspection := inspectLifecycleError(err)
		if invalidator, ok := r.job.blocks.(interface {
			InvalidateRevision(catalog.FileID, content.FileRevision)
		}); ok && inspection.invalidatedRevision {
			invalidator.InvalidateRevision(plan.file, opened.Descriptor.FileRevision())
		}
		return VerifiedDurableRanges{}, false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureBlockTransfer, err, inspection.retireReason(),
		)
	}
	if err := buffered.Flush(ctx); err != nil {
		return VerifiedDurableRanges{}, false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureBlockTransfer, err, 0,
		)
	}
	checkpoint, err := transaction.Checkpoint(ctx)
	if err != nil {
		return VerifiedDurableRanges{}, false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, err, 0,
		)
	}
	valid := checkpointExactlyAdvances(transaction, prior, requested, checkpoint)
	if !durableOutput {
		valid = checkpointAcknowledgesTransientWrite(transaction, prior, checkpoint)
	}
	if !valid {
		return VerifiedDurableRanges{}, false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0,
		)
	}
	return checkpoint, true, nil
}
