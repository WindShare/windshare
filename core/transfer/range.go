package transfer

import (
	"context"
	"errors"
	"fmt"

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
