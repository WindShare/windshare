package transfer

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
)

var (
	ErrInvalidDemand    = errors.New("receiver block demand is invalid")
	ErrBlockIdentity    = errors.New("received block changes the requested identity")
	ErrBlockInvalidated = errors.New("receiver block revision was invalidated")
	ErrBrokerClosed     = errors.New("receiver block broker is closed")
)

const (
	DefaultConcurrentBlocks = 8
	MaximumConcurrentBlocks = 64
)

type blockFetcher interface {
	fetch(context.Context, BlockDemand, func(records.BlockRecord) error) (records.BlockRecord, error)
}

type BlockBrokerConfig struct {
	ShareInstance       catalog.ShareInstance
	Lanes               *LaneSet
	MaxBytes            uint64
	ProcessBudget       *PlaintextBudget
	MaxConcurrentBlocks int
}

type blockKey struct {
	file     catalog.FileID
	revision content.FileRevision
	index    uint64
}

type cachedBlock struct {
	key  blockKey
	data []byte
}

type blockCall struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	waiters   int
	completed bool
	reserved  uint64
	data      []byte
	err       error
}

// BlockBroker is receiver-scoped: no operation, cancellation, or plaintext is
// shared across receivers. Only the sender's independently authenticated object
// cache may collapse their upstream source reads.
type BlockBroker struct {
	share               catalog.ShareInstance
	blocks              blockFetcher
	maxBytes            uint64
	maxConcurrentBlocks int
	process             *PlaintextBudget
	lifecycle           context.Context
	stop                context.CancelFunc

	mu            sync.Mutex
	loads         sync.WaitGroup
	closed        bool
	used          uint64
	inflightBytes uint64
	entries       map[blockKey]*list.Element
	lru           *list.List
	inflight      map[blockKey]*blockCall
}

func NewBlockBroker(config BlockBrokerConfig) (*BlockBroker, error) {
	if config.Lanes == nil {
		return nil, errors.New("block broker requires a lane set")
	}
	return newBlockBroker(config, config.Lanes)
}

func newBlockBroker(config BlockBrokerConfig, blocks blockFetcher) (*BlockBroker, error) {
	if config.ShareInstance.IsZero() || blocks == nil || config.ProcessBudget == nil {
		return nil, errors.New("block broker requires share identity, block fetcher, and process budget")
	}
	if config.MaxBytes == 0 {
		config.MaxBytes = DefaultSessionPlaintextBytes
	}
	if config.MaxConcurrentBlocks == 0 {
		config.MaxConcurrentBlocks = DefaultConcurrentBlocks
	}
	if config.MaxConcurrentBlocks < 1 || config.MaxConcurrentBlocks > MaximumConcurrentBlocks {
		return nil, errors.New("block broker concurrent block limit is invalid")
	}
	lifecycle, stop := context.WithCancel(context.Background())
	return &BlockBroker{
		share: config.ShareInstance, blocks: blocks, maxBytes: config.MaxBytes,
		maxConcurrentBlocks: config.MaxConcurrentBlocks, process: config.ProcessBudget,
		lifecycle: lifecycle, stop: stop, entries: make(map[blockKey]*list.Element), lru: list.New(),
		inflight: make(map[blockKey]*blockCall),
	}, nil
}

func (b *BlockBroker) GetBlock(
	ctx context.Context,
	leaseID content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	index uint64,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expected, err := validateDemand(b.share, leaseID, descriptor, index)
	if err != nil {
		return nil, err
	}
	key := blockKey{file: descriptor.FileID(), revision: descriptor.FileRevision(), index: index}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBrokerClosed
	}
	if element := b.entries[key]; element != nil {
		b.lru.MoveToFront(element)
		data := slices.Clone(element.Value.(*cachedBlock).data)
		b.mu.Unlock()
		return data, nil
	}
	call := b.inflight[key]
	if call == nil {
		if err := b.reserveLocked(uint64(expected)); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		loadContext, cancel := context.WithCancel(context.Background())
		stopLifecycle := context.AfterFunc(b.lifecycle, cancel)
		call = &blockCall{
			ctx: loadContext, cancel: func() { stopLifecycle(); cancel() }, done: make(chan struct{}),
			waiters: 1, reserved: uint64(expected),
		}
		b.inflight[key] = call
		// The broker's closed-state lock also gates worker registration, making
		// the Add/Wait boundary a lifecycle invariant instead of a timing race.
		b.loads.Add(1)
		go b.runLoad(key, call, BlockDemand{LeaseID: leaseID, Descriptor: descriptor, Index: index})
	} else {
		call.waiters++
	}
	b.mu.Unlock()
	return b.await(ctx, key, call)
}

func validateDemand(share catalog.ShareInstance, leaseID content.LeaseID, descriptor content.FileRevisionDescriptor, index uint64) (uint32, error) {
	if leaseID.IsZero() || descriptor.ShareInstance() != share || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return 0, ErrInvalidDemand
	}
	length, err := descriptor.Geometry().BlockPlainLength(index)
	if err != nil {
		return 0, errors.Join(ErrInvalidDemand, err)
	}
	return length, nil
}

func (b *BlockBroker) reserveLocked(bytes uint64) error {
	if bytes == 0 || bytes > b.maxBytes {
		return ErrPlaintextBudget
	}
	for bytes > b.maxBytes-b.used-b.inflightBytes {
		if !b.evictOldestLocked() {
			return ErrPlaintextBudget
		}
	}
	for !b.process.tryReserve(bytes) {
		if !b.evictOldestLocked() {
			return ErrPlaintextBudget
		}
	}
	b.inflightBytes += bytes
	return nil
}

func (b *BlockBroker) await(ctx context.Context, key blockKey, call *blockCall) ([]byte, error) {
	select {
	case <-call.done:
		return slices.Clone(call.data), call.err
	case <-ctx.Done():
		b.mu.Lock()
		if b.inflight[key] == call && !call.completed {
			call.waiters--
			if call.waiters == 0 {
				delete(b.inflight, key)
				b.completeAbandonedCallLocked(call, ctx.Err())
			}
		}
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (b *BlockBroker) runLoad(key blockKey, call *blockCall, demand BlockDemand) {
	defer b.loads.Done()
	record, err := b.blocks.fetch(call.ctx, demand, func(candidate records.BlockRecord) error {
		if candidate.Descriptor() != demand.Descriptor || candidate.LocalBlockIndex() != demand.Index {
			return ErrBlockIdentity
		}
		expected, lengthErr := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
		if lengthErr != nil || uint64(candidate.DataLength()) != uint64(expected) {
			return ErrBlockIdentity
		}
		return nil
	})
	var data []byte
	if err == nil {
		data = record.Data()
	}
	b.mu.Lock()
	if call.completed {
		b.mu.Unlock()
		return
	}
	call.completed = true
	active := b.inflight[key] == call
	if active {
		delete(b.inflight, key)
	}
	if err == nil && active && call.waiters > 0 && !b.closed {
		b.inflightBytes -= call.reserved
		call.reserved = 0
		b.storeReservedLocked(key, data)
	} else {
		b.releaseCallReservationLocked(call)
	}
	call.data, call.err = data, err
	close(call.done)
	call.cancel()
	b.mu.Unlock()
}

func (b *BlockBroker) releaseCallReservationLocked(call *blockCall) {
	if call.reserved == 0 {
		return
	}
	b.inflightBytes -= call.reserved
	b.process.release(call.reserved)
	call.reserved = 0
}

func (b *BlockBroker) completeAbandonedCallLocked(call *blockCall, err error) {
	if call.completed {
		return
	}
	call.completed = true
	b.releaseCallReservationLocked(call)
	call.data, call.err = nil, err
	close(call.done)
	call.cancel()
}

func (b *BlockBroker) storeReservedLocked(key blockKey, data []byte) {
	entry := &cachedBlock{key: key, data: data}
	element := b.lru.PushFront(entry)
	b.entries[key] = element
	b.used += uint64(len(data))
}

func (b *BlockBroker) evictOldestLocked() bool {
	element := b.lru.Back()
	if element == nil {
		return false
	}
	entry := element.Value.(*cachedBlock)
	delete(b.entries, entry.key)
	b.lru.Remove(element)
	bytes := uint64(len(entry.data))
	b.used -= bytes
	b.process.release(bytes)
	return true
}

func (b *BlockBroker) InvalidateRevision(file catalog.FileID, revision content.FileRevision) {
	b.mu.Lock()
	for key, element := range b.entries {
		if key.file == file && key.revision == revision {
			entry := element.Value.(*cachedBlock)
			delete(b.entries, key)
			b.lru.Remove(element)
			bytes := uint64(len(entry.data))
			b.used -= bytes
			b.process.release(bytes)
		}
	}
	for key, call := range b.inflight {
		if key.file == file && key.revision == revision {
			delete(b.inflight, key)
			b.completeAbandonedCallLocked(call, ErrBlockInvalidated)
		}
	}
	b.mu.Unlock()
}

func (b *BlockBroker) UsedBytes() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used + b.inflightBytes
}

// Stop rejects demands and cancels broker workers without waiting. A lane
// callback can use it without attempting to join the runLoad that owns it.
func (b *BlockBroker) Stop() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		for key, call := range b.inflight {
			delete(b.inflight, key)
			b.completeAbandonedCallLocked(call, ErrBrokerClosed)
		}
		for b.evictOldestLocked() {
		}
	}
	b.mu.Unlock()
	b.stop()
}

// Close is the external ownership boundary. Callbacks terminate through Stop
// because a synchronous join from owned work cannot complete itself.
func (b *BlockBroker) Close() {
	b.Stop()
	// The broker releases admission before joining so callers unblock promptly,
	// while ownership still does not end until every broker worker has exited.
	b.loads.Wait()
}
