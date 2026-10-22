package transfer

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/transfer/bufferbudget"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

var (
	ErrInvalidDemand    = errors.New("receiver block demand is invalid")
	ErrBlockIdentity    = errors.New("received block changes the requested identity")
	ErrBlockInvalidated = errors.New("receiver block revision was invalidated")
	ErrBrokerClosed     = errors.New("receiver block broker is closed")
	ErrPlaintextBudget  = errors.New("receiver plaintext budget exceeded")
)

const (
	DefaultConcurrentBlocks      = 8
	MaximumConcurrentBlocks      = 64
	DefaultSessionPlaintextBytes = uint64(128) << 20
	DefaultProcessPlaintextBytes = uint64(1) << 30
)

// PlaintextBudget is shared by receivers without sharing their plaintext.
type PlaintextBudget = bufferbudget.Budget

func NewPlaintextBudget(limit uint64) (*PlaintextBudget, error) {
	return bufferbudget.New(limit)
}

type authenticatedBlock struct {
	records.BlockRecord
	route downloadmetrics.Route
}
type deliveredBlock struct {
	data    []byte
	route   downloadmetrics.Route
	release func()
}
type blockClaim func() (release func(), changed <-chan struct{}, err error)

type blockRequest struct {
	demand   BlockDemand
	validate func(records.BlockRecord) error
	claim    blockClaim
}

type blockFetcher interface {
	fetch(context.Context, blockRequest) (authenticatedBlock, error)
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
	route  downloadmetrics.Route
	buffer *bufferbudget.Cached[blockKey]
}

type blockConsumer struct {
	ready    func() bool
	admitted func()
}

type blockCall struct {
	route            downloadmetrics.Route
	ctx              context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	waiters          int
	consumers        map[*blockConsumer]struct{}
	completed        bool
	workerDone       bool
	reserved         uint64
	deliveryReserved uint64
	expected         uint64
	activeAttempts   int
	data             []byte
	err              error
}

// BlockBroker is receiver-scoped: no operation, cancellation, or plaintext is
// shared across receivers. Only the sender's independently authenticated object
// cache may collapse their upstream source reads.
type BlockBroker struct {
	lanes               *LaneSet
	share               catalog.ShareInstance
	blocks              blockFetcher
	maxBytes            uint64
	maxConcurrentBlocks int
	process             *PlaintextBudget
	account             *bufferbudget.Account[blockKey]
	lifecycle           context.Context
	stop                context.CancelFunc

	mu             sync.Mutex
	loads          sync.WaitGroup
	closed         bool
	activeAttempts int
	entries        map[blockKey]*cachedBlock
	inflight       map[blockKey]*blockCall
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
		share: config.ShareInstance, blocks: blocks, lanes: config.Lanes, maxBytes: config.MaxBytes,
		maxConcurrentBlocks: config.MaxConcurrentBlocks, process: config.ProcessBudget,
		lifecycle: lifecycle, stop: stop, entries: make(map[blockKey]*cachedBlock), account: bufferbudget.NewAccount[blockKey](config.ProcessBudget, config.MaxBytes),
		inflight: make(map[blockKey]*blockCall),
	}, nil
}

func (b *BlockBroker) GetBlock(ctx context.Context, leaseID content.LeaseID, descriptor content.FileRevisionDescriptor, index uint64) ([]byte, error) {
	block, err := b.getBlock(ctx, leaseID, descriptor, index, nil)
	if block.release != nil {
		defer block.release()
	}
	return block.data, err
}
func (b *BlockBroker) getBlock(
	ctx context.Context,
	leaseID content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	index uint64,
	consumer *blockConsumer,
) (deliveredBlock, error) {
	if err := ctx.Err(); err != nil {
		return deliveredBlock{}, err
	}
	expected, err := validateDemand(b.share, leaseID, descriptor, index)
	if err != nil {
		return deliveredBlock{}, err
	}
	if uint64(expected)*3 > b.maxBytes || uint64(expected)*3 > b.process.Limit() {
		return deliveredBlock{}, ErrPlaintextBudget
	}
	key := blockKey{file: descriptor.FileID(), revision: descriptor.FileRevision(), index: index}
	if consumer == nil {
		consumer = &blockConsumer{ready: func() bool { return true }, admitted: func() {}}
	}
	for {
		changed := b.process.Changes()
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return deliveredBlock{}, ErrBrokerClosed
		}
		block, waiting := b.copyCachedLocked(key, consumer)
		if block.data != nil {
			b.mu.Unlock()
			return block, nil
		}
		if waiting {
			b.mu.Unlock()
			if err := b.waitCapacity(ctx, changed); err != nil {
				return deliveredBlock{}, err
			}
			continue
		}
		call := b.inflight[key]
		if call == nil {
			loadContext, cancel := context.WithCancel(context.Background())
			stopLifecycle := context.AfterFunc(b.lifecycle, cancel)
			call = &blockCall{
				ctx: loadContext, cancel: func() { stopLifecycle(); cancel() }, done: make(chan struct{}),
				waiters: 1, expected: uint64(expected), consumers: map[*blockConsumer]struct{}{consumer: {}},
			}
			b.inflight[key] = call
			// The broker's closed-state lock also gates worker registration, making
			// the Add/Wait boundary a lifecycle invariant instead of a timing race.
			b.loads.Add(1)
			go b.runLoad(call, BlockDemand{LeaseID: leaseID, Descriptor: descriptor, Index: index})
		} else {
			call.waiters++
			call.consumers[consumer] = struct{}{}
		}
		b.mu.Unlock()
		return b.await(ctx, key, call, consumer)
	}
}

func (b *BlockBroker) copyCachedLocked(key blockKey, consumer *blockConsumer) (deliveredBlock, bool) {
	cached := b.entries[key]
	if cached == nil {
		return deliveredBlock{}, false
	}
	if !consumer.ready() {
		return deliveredBlock{}, true
	}
	data, live := cached.buffer.Copy()
	if !live {
		delete(b.entries, key)
		return deliveredBlock{}, false
	}
	if data == nil {
		return deliveredBlock{}, true
	}
	consumer.admitted()
	return deliveredBlock{data: data, route: cached.route, release: b.releaseDelivery(uint64(len(data)))}, false
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
	if !b.account.Reserve(bytes) {
		return ErrPlaintextBudget
	}
	return nil
}

func (b *BlockBroker) waitCapacity(ctx context.Context, changed <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.lifecycle.Done():
		return ErrBrokerClosed
	case <-changed:
		return nil
	}
}

func (b *BlockBroker) await(ctx context.Context, key blockKey, call *blockCall, consumer *blockConsumer) (deliveredBlock, error) {
	defer b.leaveCall(key, call, consumer)
	select {
	case <-call.done:
		return b.deliverCall(ctx, call, consumer)
	case <-ctx.Done():
		return deliveredBlock{}, ctx.Err()
	}
}

func (b *BlockBroker) deliverCall(ctx context.Context, call *blockCall, consumer *blockConsumer) (deliveredBlock, error) {
	for {
		changed := b.process.Changes()
		b.mu.Lock()
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return deliveredBlock{}, err
		}
		if call.err != nil {
			b.mu.Unlock()
			return deliveredBlock{}, call.err
		}
		if !consumer.ready() {
			b.mu.Unlock()
			if err := b.waitCapacity(ctx, changed); err != nil {
				return deliveredBlock{}, err
			}
			continue
		}
		bytes := uint64(len(call.data))
		reserved := call.deliveryReserved > 0
		if reserved {
			call.deliveryReserved = 0
		} else {
			reserved = b.reserveLocked(bytes) == nil
		}
		if reserved {
			consumer.admitted()
			block := deliveredBlock{data: slices.Clone(call.data), route: call.route, release: b.releaseDelivery(bytes)}
			b.mu.Unlock()
			return block, nil
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return deliveredBlock{}, ctx.Err()
		case <-b.lifecycle.Done():
			return deliveredBlock{}, ErrBrokerClosed
		case <-changed:
		}
	}
}

func (b *BlockBroker) releaseDelivery(bytes uint64) func() {
	return sync.OnceFunc(func() {
		b.mu.Lock()
		b.account.Release(bytes)
		b.mu.Unlock()
	})
}

func (b *BlockBroker) leaveCall(key blockKey, call *blockCall, consumer *blockConsumer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call.waiters--
	delete(call.consumers, consumer)
	if call.waiters != 0 {
		return
	}
	active := b.inflight[key] == call
	if active {
		delete(b.inflight, key)
	}
	if !call.completed {
		b.completeAbandonedCallLocked(call, context.Canceled)
		return
	}
	if !call.workerDone {
		return
	}
	if active && !b.closed && call.err == nil && call.reserved > 0 {
		call.reserved = 0
		b.storeReservedLocked(key, call.data, call.route)
	}
	b.releaseCallReservationLocked(call)
}

func (b *BlockBroker) claimCall(call *blockCall) (func(), <-chan struct{}, error) {
	changed := b.process.Changes()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, ErrBrokerClosed
	}
	if err := call.ctx.Err(); err != nil {
		return nil, nil, err
	}
	limit := b.maxConcurrentBlocks
	if call.activeAttempts > 0 {
		limit += lanescheduling.MaximumSupplements
	}
	if b.activeAttempts >= limit {
		return nil, changed, nil
	}
	bytes := call.expected
	var consumer *blockConsumer
	if call.reserved == 0 {
		for candidate := range call.consumers {
			if candidate.ready() {
				consumer = candidate
				break
			}
		}
		if consumer == nil {
			return nil, changed, nil
		}
		// Canonical content, the first independent delivery, and this attempt have
		// separate lifetimes. A canceled hedge cannot lend its pending buffers.
		bytes += 2 * call.expected
	}
	if err := b.reserveLocked(bytes); err != nil {
		return nil, changed, nil
	}
	if consumer != nil {
		call.reserved, call.deliveryReserved = call.expected, call.expected
		consumer.admitted()
	}
	b.activeAttempts++
	call.activeAttempts++
	return sync.OnceFunc(func() {
		b.mu.Lock()
		b.activeAttempts--
		call.activeAttempts--
		b.account.Release(call.expected)
		b.mu.Unlock()
	}), nil, nil
}

func (b *BlockBroker) runLoad(call *blockCall, demand BlockDemand) {
	defer b.loads.Done()
	record, err := b.blocks.fetch(call.ctx, blockRequest{demand: demand, claim: func() (func(), <-chan struct{}, error) { return b.claimCall(call) }, validate: func(candidate records.BlockRecord) error {
		if candidate.Descriptor() != demand.Descriptor || candidate.LocalBlockIndex() != demand.Index {
			return ErrBlockIdentity
		}
		expected, lengthErr := demand.Descriptor.Geometry().BlockPlainLength(demand.Index)
		if lengthErr != nil || uint64(candidate.DataLength()) != uint64(expected) {
			return ErrBlockIdentity
		}
		return nil
	}})
	var data []byte
	if err == nil {
		data = record.Data()
	}
	b.mu.Lock()
	call.workerDone = true
	if call.completed {
		b.releaseCallReservationLocked(call)
		b.mu.Unlock()
		return
	}
	call.completed = true
	call.data, call.err, call.route = data, err, record.route
	close(call.done)
	call.cancel()
	b.mu.Unlock()
}

func (b *BlockBroker) releaseCallReservationLocked(call *blockCall) {
	bytes := call.reserved + call.deliveryReserved
	if bytes == 0 {
		return
	}
	b.account.Release(bytes)
	call.reserved, call.deliveryReserved = 0, 0
}

func (b *BlockBroker) completeAbandonedCallLocked(call *blockCall, err error) {
	if call.completed && call.err != nil {
		return
	}
	if !call.completed {
		close(call.done)
	}
	call.completed = true
	// Logical cancellation wakes consumers, but admitted workers and their
	// buffers keep their reservations until physical settlement.
	call.data, call.err = nil, err
	call.cancel()
	if call.workerDone {
		b.releaseCallReservationLocked(call)
	}
}

func (b *BlockBroker) storeReservedLocked(key blockKey, data []byte, route downloadmetrics.Route) {
	b.pruneEvictedLocked()
	b.entries[key] = &cachedBlock{route: route, buffer: b.account.Store(key, data)}
}

func (b *BlockBroker) pruneEvictedLocked() {
	for _, evicted := range b.account.TakeEvicted() {
		key := evicted.Key()
		if cached := b.entries[key]; cached != nil && cached.buffer == evicted {
			delete(b.entries, key)
		}
	}
}

func (b *BlockBroker) InvalidateRevision(file catalog.FileID, revision content.FileRevision) {
	b.mu.Lock()
	b.pruneEvictedLocked()
	for key, cached := range b.entries {
		if key.file == file && key.revision == revision {
			delete(b.entries, key)
			cached.buffer.Drop()
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
	return b.account.Used()
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
		for key, cached := range b.entries {
			cached.buffer.Drop()
			delete(b.entries, key)
		}
		b.account.TakeEvicted()
	}
	b.mu.Unlock()
	b.stop()
}

// Close is the external ownership boundary. Callbacks terminate through Stop
// because a synchronous join from owned work cannot complete itself.
func (b *BlockBroker) Close() {
	b.Stop()
	// Consumers unblock on Stop; admitted buffers remain charged until their workers exit.
	b.loads.Wait()
}
