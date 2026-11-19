package contentflow

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

const (
	DefaultShareBlockCacheBytes   = uint64(256) << 20
	DefaultProcessBlockCacheBytes = uint64(2) << 30
)

var (
	ErrInvalidCacheKey     = errors.New("block payload cache key is invalid")
	errBlockCacheIntegrity = errors.New("cached block payload differs from its committed record")
)

type BlockCacheKey struct {
	ShareInstance   catalog.ShareInstance
	FileID          catalog.FileID
	FileRevision    content.FileRevision
	LocalBlockIndex uint64
}

type revisionCacheIdentity struct {
	file     catalog.FileID
	revision content.FileRevision
}

func (key BlockCacheKey) revisionIdentity() revisionCacheIdentity {
	return revisionCacheIdentity{file: key.FileID, revision: key.FileRevision}
}

type revisionWatch struct {
	cancel context.CancelCauseFunc
}

func NewBlockCacheKey(descriptor content.FileRevisionDescriptor, index uint64) (BlockCacheKey, error) {
	if descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return BlockCacheKey{}, ErrInvalidCacheKey
	}
	if _, err := descriptor.Geometry().BlockPlainLength(index); err != nil {
		return BlockCacheKey{}, ErrInvalidCacheKey
	}
	return BlockCacheKey{
		ShareInstance: descriptor.ShareInstance(), FileID: descriptor.FileID(),
		FileRevision: descriptor.FileRevision(), LocalBlockIndex: index,
	}, nil
}

type cachedBlock struct {
	payload  []byte
	recordID records.RecordID
}

func (block cachedBlock) clone() cachedBlock {
	return cachedBlock{payload: slices.Clone(block.payload), recordID: block.recordID}
}

type cacheEntry struct {
	key   BlockCacheKey
	block cachedBlock
	bytes uint64
}

type cacheCall struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	waiters   int
	completed bool
	block     cachedBlock
	err       error
}

type SharedBlockCache struct {
	share       catalog.ShareInstance
	maxBytes    uint64
	reservation cacheReservation

	mu       sync.Mutex
	loads    sync.WaitGroup
	closed   bool
	used     uint64
	entries  map[BlockCacheKey]*list.Element
	lru      *list.List
	inflight map[BlockCacheKey]*cacheCall
	invalid  map[revisionCacheIdentity]struct{}
	watches  map[revisionCacheIdentity]map[*revisionWatch]struct{}
}

func NewSharedBlockCache(share catalog.ShareInstance, maxBytes uint64, process *ProcessCacheBudget) (*SharedBlockCache, error) {
	if share.IsZero() || process == nil {
		return nil, errors.New("block payload cache requires share identity and process budget")
	}
	if maxBytes == 0 {
		maxBytes = DefaultShareBlockCacheBytes
	}
	return &SharedBlockCache{
		share: share, maxBytes: maxBytes, reservation: cacheReservation{budget: process},
		entries: make(map[BlockCacheKey]*list.Element), lru: list.New(), inflight: make(map[BlockCacheKey]*cacheCall),
		invalid: make(map[revisionCacheIdentity]struct{}), watches: make(map[revisionCacheIdentity]map[*revisionWatch]struct{}),
	}, nil
}

func (c *SharedBlockCache) Get(ctx context.Context, key BlockCacheKey, load func(context.Context) ([]byte, error)) ([]byte, error) {
	block, err := c.get(ctx, key, load)
	return block.payload, err
}

func (c *SharedBlockCache) get(ctx context.Context, key BlockCacheKey, load func(context.Context) ([]byte, error)) (cachedBlock, error) {
	if err := ctx.Err(); err != nil {
		return cachedBlock{}, err
	}
	if load == nil || key.ShareInstance != c.share || key.FileID.IsZero() || key.FileRevision.IsZero() {
		return cachedBlock{}, ErrInvalidCacheKey
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return cachedBlock{}, ErrServiceClosed
	}
	if _, invalid := c.invalid[key.revisionIdentity()]; invalid {
		c.mu.Unlock()
		return cachedBlock{}, content.ErrRevisionDrift
	}
	if element := c.entries[key]; element != nil {
		c.lru.MoveToFront(element)
		block := element.Value.(*cacheEntry).block.clone()
		c.mu.Unlock()
		// The commitment was fixed by the authenticated revision read. Do not
		// generate a new identity for stale or corrupted cache bytes.
		if !recordDigestMatches(block.recordID, block.payload) {
			c.mu.Lock()
			if c.entries[key] == element {
				c.removeEntryLocked(element)
			}
			c.mu.Unlock()
			return cachedBlock{}, errBlockCacheIntegrity
		}
		return block, nil
	}
	call := c.inflight[key]
	if call == nil {
		callContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &cacheCall{ctx: callContext, cancel: cancel, done: make(chan struct{}), waiters: 1}
		c.inflight[key] = call
		// Registration shares the closed-state lock so Close can never begin
		// waiting while a newly admitted load is still able to increment the group.
		c.loads.Add(1)
		go c.runLoad(key, call, load)
	} else {
		call.waiters++
	}
	c.mu.Unlock()
	return c.await(ctx, key, call)
}

func (c *SharedBlockCache) watchRevision(
	descriptor content.FileRevisionDescriptor,
	cancel context.CancelCauseFunc,
) (func(), error) {
	if cancel == nil || descriptor.ShareInstance() != c.share || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return nil, ErrInvalidCacheKey
	}
	identity := revisionCacheIdentity{file: descriptor.FileID(), revision: descriptor.FileRevision()}
	watch := &revisionWatch{cancel: cancel}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrServiceClosed
	}
	if _, invalid := c.invalid[identity]; invalid {
		c.mu.Unlock()
		return nil, content.ErrRevisionDrift
	}
	if c.watches[identity] == nil {
		c.watches[identity] = make(map[*revisionWatch]struct{})
	}
	c.watches[identity][watch] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if watches := c.watches[identity]; watches != nil {
				delete(watches, watch)
				if len(watches) == 0 {
					delete(c.watches, identity)
				}
			}
			c.mu.Unlock()
		})
	}, nil
}

func (c *SharedBlockCache) await(ctx context.Context, key BlockCacheKey, call *cacheCall) (cachedBlock, error) {
	select {
	case <-call.done:
		return call.block.clone(), call.err
	case <-ctx.Done():
		c.mu.Lock()
		if c.inflight[key] == call && !call.completed {
			call.waiters--
			if call.waiters == 0 {
				delete(c.inflight, key)
				c.completeTerminatedCallLocked(call, ctx.Err())
			}
		}
		c.mu.Unlock()
		return cachedBlock{}, ctx.Err()
	}
}

func (c *SharedBlockCache) runLoad(key BlockCacheKey, call *cacheCall, load func(context.Context) ([]byte, error)) {
	defer c.loads.Done()
	payload, err := load(call.ctx)
	var block cachedBlock
	if err == nil {
		if len(payload) == 0 {
			err = errors.New("block loader returned an empty payload")
		} else {
			block.payload = slices.Clone(payload)
			block.recordID = records.RecordIDFromObject(block.payload)
		}
	}
	c.mu.Lock()
	if call.completed {
		c.mu.Unlock()
		return
	}
	call.completed = true
	active := c.inflight[key] == call
	if active {
		delete(c.inflight, key)
	}
	// Invalidation removes the call before cancelling it. A loader that ignores
	// cancellation must not repopulate the cache with the invalidated revision.
	if err == nil && active && call.waiters > 0 && !c.closed {
		c.storeLocked(key, block)
	}
	call.block, call.err = block, err
	close(call.done)
	call.cancel()
	c.mu.Unlock()
}

func (c *SharedBlockCache) completeTerminatedCallLocked(call *cacheCall, err error) {
	if call.completed {
		return
	}
	call.completed = true
	call.block, call.err = cachedBlock{}, err
	close(call.done)
	call.cancel()
}

func (c *SharedBlockCache) storeLocked(key BlockCacheKey, block cachedBlock) {
	bytes := uint64(len(block.payload))
	if bytes > c.maxBytes {
		return
	}
	for bytes > c.maxBytes-c.used {
		if !c.evictOldestLocked() {
			return
		}
	}
	if !c.reservation.grow(bytes) {
		return
	}
	entry := &cacheEntry{key: key, block: block, bytes: bytes}
	element := c.lru.PushFront(entry)
	c.entries[key] = element
	c.used += bytes
}

func (c *SharedBlockCache) evictOldestLocked() bool {
	element := c.lru.Back()
	if element == nil {
		return false
	}
	c.removeEntryLocked(element)
	return true
}

func (c *SharedBlockCache) removeEntryLocked(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
	c.used -= entry.bytes
	c.reservation.shrink(entry.bytes)
}

func (c *SharedBlockCache) InvalidateRevision(file catalog.FileID, revision content.FileRevision) {
	identity := revisionCacheIdentity{file: file, revision: revision}
	c.mu.Lock()
	c.invalid[identity] = struct{}{}
	for key, element := range c.entries {
		if key.FileID == file && key.FileRevision == revision {
			c.removeEntryLocked(element)
		}
	}
	for key, call := range c.inflight {
		if key.FileID == file && key.FileRevision == revision {
			delete(c.inflight, key)
			c.completeTerminatedCallLocked(call, content.ErrRevisionDrift)
		}
	}
	watches := make([]*revisionWatch, 0, len(c.watches[identity]))
	for watch := range c.watches[identity] {
		watches = append(watches, watch)
	}
	delete(c.watches, identity)
	c.mu.Unlock()
	for _, watch := range watches {
		watch.cancel(content.ErrRevisionDrift)
	}
}

func (c *SharedBlockCache) UsedBytes() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Stop rejects new work and cancels current work without waiting. Loader
// callbacks use Stop when they must end the cache from inside a counted load.
func (c *SharedBlockCache) Stop() {
	c.mu.Lock()
	watches := make([]*revisionWatch, 0)
	if !c.closed {
		c.closed = true
		for key, call := range c.inflight {
			delete(c.inflight, key)
			c.completeTerminatedCallLocked(call, ErrServiceClosed)
		}
		for c.evictOldestLocked() {
		}
		for identity, revisionWatches := range c.watches {
			for watch := range revisionWatches {
				watches = append(watches, watch)
			}
			delete(c.watches, identity)
		}
		clear(c.invalid)
	}
	c.mu.Unlock()
	for _, watch := range watches {
		watch.cancel(ErrServiceClosed)
	}
}

// Close is the external ownership boundary. A loader that needs to terminate
// its owner calls Stop; joining from that same callback cannot make progress.
func (c *SharedBlockCache) Close() {
	c.Stop()
	// A closed cache owns no loader execution; joining here makes teardown of
	// revision sources and keys safe even when a loader delayed cancellation.
	c.loads.Wait()
}
