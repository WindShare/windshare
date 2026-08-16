package transfer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type rangeBlockResult struct {
	index uint64
	data  []byte
	err   error
}

const (
	// Keep the output transaction atomic at the same bounded window the broker
	// can keep in flight. A wider request would only add receiver memory because
	// the broker cannot make additional upstream progress concurrently.
	defaultFileReadWindowBlocks     = DefaultConcurrentBlocks
	maximumAtomicRequestedRangeSize = uint64(defaultFileReadWindowBlocks * catalog.MaxChunkSize)
)

type RangeSink interface {
	WriteRange(context.Context, uint64, []byte) error
}

type RangeSinkFunc func(context.Context, uint64, []byte) error

func (function RangeSinkFunc) WriteRange(ctx context.Context, offset uint64, data []byte) error {
	return function(ctx, offset, data)
}

type atomicRequestedRangeSink struct {
	mu        sync.Mutex
	target    RangeSink
	requested content.Range
	data      []byte
	covered   []content.Range
	count     uint64
	failure   error
	sealed    bool
}

func newAtomicRequestedRangeSink(
	requested content.Range,
	target RangeSink,
) (*atomicRequestedRangeSink, error) {
	if target == nil || requested.Offset >= requested.End {
		return nil, rangeReaderContractError(errors.New("requested range is empty or has no output sink"))
	}
	length := requested.End - requested.Offset
	maxInt := uint64(^uint(0) >> 1)
	if length > maximumAtomicRequestedRangeSize || length > maxInt {
		return nil, rangeReaderContractError(errors.New("requested range exceeds the atomic protocol bound"))
	}
	return &atomicRequestedRangeSink{
		target: target, requested: requested,
		data: make([]byte, int(length)),
	}, nil
}

func (sink *atomicRequestedRangeSink) WriteRange(
	ctx context.Context,
	offset uint64,
	data []byte,
) error {
	if err := ctx.Err(); err != nil {
		sink.mu.Lock()
		if sink.failure == nil {
			sink.failure = err
		}
		sink.mu.Unlock()
		return err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.failure != nil {
		return sink.failure
	}
	if sink.sealed {
		return sink.failContractLocked(errors.New("range reader wrote after returning"))
	}
	if len(data) == 0 || offset < sink.requested.Offset || offset >= sink.requested.End ||
		uint64(len(data)) > sink.requested.End-offset {
		return sink.failContractLocked(errors.New("range write escapes the requested interval"))
	}
	start := offset - sink.requested.Offset
	end := start + uint64(len(data))
	if !sink.admitCoverageLocked(content.Range{Offset: start, End: end}) {
		return sink.failContractLocked(errors.New("range write overlaps bytes already supplied"))
	}
	copy(sink.data[int(start):int(end)], data)
	sink.count += uint64(len(data))
	return nil
}

func (sink *atomicRequestedRangeSink) admitCoverageLocked(candidate content.Range) bool {
	index := sort.Search(len(sink.covered), func(index int) bool {
		return sink.covered[index].Offset >= candidate.Offset
	})
	mergeLeft := index > 0 && sink.covered[index-1].End == candidate.Offset
	if index > 0 && sink.covered[index-1].End > candidate.Offset {
		return false
	}
	mergeRight := index < len(sink.covered) && sink.covered[index].Offset == candidate.End
	if index < len(sink.covered) && sink.covered[index].Offset < candidate.End {
		return false
	}
	first, last := index, index
	if mergeLeft {
		first--
		candidate.Offset = sink.covered[first].Offset
	}
	if mergeRight {
		candidate.End = sink.covered[index].End
		last++
	}
	// Canonical intervals make validation proportional to collaborator writes,
	// not bytes. A bitmap would turn every 32 MiB pipeline window into tens of
	// millions of bookkeeping operations before any output I/O could begin.
	updated := make([]content.Range, 0, len(sink.covered)+1-(last-first))
	updated = append(updated, sink.covered[:first]...)
	updated = append(updated, candidate)
	updated = append(updated, sink.covered[last:]...)
	sink.covered = updated
	return true
}

func (sink *atomicRequestedRangeSink) Flush(ctx context.Context) error {
	if sink == nil {
		return rangeReaderContractError(errors.New("range reader returned no atomic sink"))
	}
	sink.mu.Lock()
	if sink.failure != nil {
		err := sink.failure
		sink.mu.Unlock()
		return err
	}
	if sink.sealed {
		err := sink.failContractLocked(errors.New("requested range was flushed more than once"))
		sink.mu.Unlock()
		return err
	}
	if sink.count != uint64(len(sink.data)) {
		err := sink.failContractLocked(errors.New("range reader returned before covering the requested interval"))
		sink.mu.Unlock()
		return err
	}
	sink.sealed = true
	target, offset, data := sink.target, sink.requested.Offset, sink.data
	sink.mu.Unlock()
	return normalizeOutputBoundary(ctx, target.WriteRange(ctx, offset, data))
}

func (sink *atomicRequestedRangeSink) Failure() error {
	if sink == nil {
		return rangeReaderContractError(errors.New("range reader returned no atomic sink"))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.failure
}

func (sink *atomicRequestedRangeSink) failContractLocked(cause error) error {
	if sink.failure == nil {
		sink.failure = rangeReaderContractError(cause)
	}
	return sink.failure
}

func rangeReaderContractError(cause error) error {
	return dependencyContractFailure(errors.Join(errRangeReaderContract, cause))
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
	progress *fileTransferProgress,
) (bool, error) {
	durableOutput := r.output.Capabilities().Durability != DurabilityNone
	if progress == nil || progress.durable != durableOutput {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
		)
	}
	if !durableOutput && !progress.trusted.Ranges().IsEmpty() {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
		)
	}
	missing, err := MissingRanges(opened.Descriptor.ExactSize(), progress.trusted.Ranges())
	if err != nil {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(err), 0, progress,
		)
	}
	wrote := false
	chunk := uint64(opened.Descriptor.Geometry().ChunkSize())
	for _, current := range missing.Ranges() {
		for offset := current.Offset; offset < current.End; {
			if cause := context.Cause(ctx); cause != nil {
				return false, r.settleFailedFile(
					ctx, plan, opened, transaction, FailureBlockTransfer,
					cancellationFailure(ctx, cause), 0, progress,
				)
			}
			// RangeReader owns the bounded block pipeline. Passing one chunk at a
			// time here serialized the production job even though BlockBroker and
			// LaneSet were already designed for concurrent, cross-lane dispatch.
			next := min(
				current.End,
				((offset/chunk)+uint64(defaultFileReadWindowBlocks))*chunk,
			)
			requested := content.Range{Offset: offset, End: next}
			if !durableOutput && requested.Offset != progress.transientEnd {
				return false, r.settleFailedFile(
					ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
				)
			}
			transferred, transferErr := r.transferRequestedRange(
				ctx, plan, opened, transaction, progress, requested,
			)
			if transferErr != nil || !transferred {
				return false, transferErr
			}
			if !wrote {
				r.traceFileLifecycle(TransferFileFirstWrite, plan, nil)
				wrote = true
			}
			offset = next
		}
	}
	complete := RangesCoverFile(opened.Descriptor.ExactSize(), progress.trusted.Ranges())
	if !durableOutput {
		complete = progress.transientEnd == opened.Descriptor.ExactSize()
	}
	if !complete {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
		)
	}
	return true, nil
}

func (r *jobRun) transferRequestedRange(
	ctx context.Context,
	plan plannedFile,
	opened OpenedRevision,
	transaction FileTransaction,
	progress *fileTransferProgress,
	requested content.Range,
) (bool, error) {
	buffered, err := newAtomicRequestedRangeSink(requested, transaction)
	if err == nil {
		rawReadErr := r.job.blocks.ReadRange(ctx, opened.LeaseID, opened.Descriptor, requested, buffered)
		err = normalizeSourceBoundary(ctx, rawReadErr)
	}
	if bufferedErr := buffered.Failure(); bufferedErr != nil {
		err = bufferedErr
	}
	if err != nil {
		policy := lifecyclePolicyFor(err)
		if invalidator, ok := r.job.blocks.(interface {
			InvalidateRevision(catalog.FileID, content.FileRevision)
		}); ok && policy.invalidatedRevision() {
			invalidator.InvalidateRevision(plan.file, opened.Descriptor.FileRevision())
		}
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureBlockTransfer, err, policy.retireReason(), progress,
		)
	}
	if err := buffered.Flush(ctx); err != nil {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureBlockTransfer, err, 0, progress,
		)
	}
	if !progress.beginPending(requested) {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
		)
	}
	checkpoint, rawCheckpointErr := transaction.Checkpoint(ctx)
	err = normalizeOutputBoundary(ctx, rawCheckpointErr)
	if err != nil {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, err, 0, progress,
		)
	}
	delta, valid := progress.acknowledge(transaction, checkpoint)
	if !valid {
		return false, r.settleFailedFile(
			ctx, plan, opened, transaction, FailureFileOutput, outputContractFault(nil), 0, progress,
		)
	}
	r.job.progress.addNewlyVerified(delta)
	return true, nil
}

func checkpointExactlyAdvances(
	transaction FileTransaction,
	prior VerifiedDurableRanges,
	requested content.Range,
	next VerifiedDurableRanges,
) bool {
	requestedSet, err := content.NewRangeSet([]content.Range{requested})
	if err != nil {
		return false
	}
	expected, err := MergeRanges(prior.Ranges(), requestedSet)
	return err == nil && next.Binding() == transaction.Binding() &&
		next.CheckpointGeneration() > prior.CheckpointGeneration() &&
		exactRangeSetsEqual(next.Ranges(), expected)
}

func checkpointAcknowledgesTransientWrite(
	transaction FileTransaction,
	prior VerifiedDurableRanges,
	next VerifiedDurableRanges,
) bool {
	return next.Binding() == transaction.Binding() && next.Ranges().IsEmpty() &&
		next.CheckpointGeneration() > prior.CheckpointGeneration()
}

func exactRangeSetsEqual(left, right content.RangeSet) bool {
	leftRanges, rightRanges := left.Ranges(), right.Ranges()
	if len(leftRanges) != len(rightRanges) {
		return false
	}
	for index := range leftRanges {
		if leftRanges[index] != rightRanges[index] {
			return false
		}
	}
	return true
}

func rangesContain(available, required content.RangeSet) bool {
	availableRanges := available.Ranges()
	availableIndex := 0
	for _, requiredRange := range required.Ranges() {
		for availableIndex < len(availableRanges) && availableRanges[availableIndex].End < requiredRange.End {
			availableIndex++
		}
		if availableIndex == len(availableRanges) || availableRanges[availableIndex].Offset > requiredRange.Offset ||
			availableRanges[availableIndex].End < requiredRange.End {
			return false
		}
	}
	return true
}
