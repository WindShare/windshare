package contentflow

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const (
	DefaultShareSealedCacheBytes   = uint64(256) << 20
	DefaultProcessSealedCacheBytes = uint64(2) << 30
)

var ErrInvalidCacheKey = errors.New("sealed block cache key is invalid")

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

type ProcessCacheBudget struct {
	mu    sync.Mutex
	limit uint64
	used  uint64
}

func NewProcessCacheBudget(limit uint64) (*ProcessCacheBudget, error) {
	if limit == 0 {
		return nil, errors.New("process cache budget must be positive")
	}
	return &ProcessCacheBudget{limit: limit}, nil
}

func (b *ProcessCacheBudget) tryReserve(bytes uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes > b.limit-b.used {
		return false
	}
	b.used += bytes
	return true
}

func (b *ProcessCacheBudget) release(bytes uint64) {
	b.mu.Lock()
	b.used -= bytes
	b.mu.Unlock()
}

func (b *ProcessCacheBudget) Used() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

type cacheEntry struct {
	key    BlockCacheKey
	object []byte
	bytes  uint64
}

type cacheCall struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	waiters   int
	completed bool
	object    []byte
	err       error
}

type SharedBlockCache struct {
	share    catalog.ShareInstance
	maxBytes uint64
	process  *ProcessCacheBudget

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
		return nil, errors.New("sealed block cache requires share identity and process budget")
	}
	if maxBytes == 0 {
		maxBytes = DefaultShareSealedCacheBytes
	}
	return &SharedBlockCache{
		share: share, maxBytes: maxBytes, process: process,
		entries: make(map[BlockCacheKey]*list.Element), lru: list.New(), inflight: make(map[BlockCacheKey]*cacheCall),
		invalid: make(map[revisionCacheIdentity]struct{}), watches: make(map[revisionCacheIdentity]map[*revisionWatch]struct{}),
	}, nil
}

func (c *SharedBlockCache) Get(ctx context.Context, key BlockCacheKey, load func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if load == nil || key.ShareInstance != c.share || key.FileID.IsZero() || key.FileRevision.IsZero() {
		return nil, ErrInvalidCacheKey
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrServiceClosed
	}
	if _, invalid := c.invalid[key.revisionIdentity()]; invalid {
		c.mu.Unlock()
		return nil, content.ErrRevisionDrift
	}
	if element := c.entries[key]; element != nil {
		c.lru.MoveToFront(element)
		object := slices.Clone(element.Value.(*cacheEntry).object)
		c.mu.Unlock()
		return object, nil
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

func (c *SharedBlockCache) await(ctx context.Context, key BlockCacheKey, call *cacheCall) ([]byte, error) {
	select {
	case <-call.done:
		return slices.Clone(call.object), call.err
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
		return nil, ctx.Err()
	}
}

func (c *SharedBlockCache) runLoad(key BlockCacheKey, call *cacheCall, load func(context.Context) ([]byte, error)) {
	defer c.loads.Done()
	object, err := load(call.ctx)
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
	if err == nil && len(object) == 0 {
		err = errors.New("sealed block loader returned an empty object")
	}
	// Invalidation removes the call before cancelling it. A loader that ignores
	// cancellation must not repopulate the cache with the invalidated revision.
	if err == nil && active && call.waiters > 0 && !c.closed {
		c.storeLocked(key, object)
	}
	call.object, call.err = slices.Clone(object), err
	close(call.done)
	call.cancel()
	c.mu.Unlock()
}

func (c *SharedBlockCache) completeTerminatedCallLocked(call *cacheCall, err error) {
	if call.completed {
		return
	}
	call.completed = true
	call.object, call.err = nil, err
	close(call.done)
	call.cancel()
}

func (c *SharedBlockCache) storeLocked(key BlockCacheKey, object []byte) {
	bytes := uint64(len(object))
	if bytes > c.maxBytes {
		return
	}
	for bytes > c.maxBytes-c.used {
		if !c.evictOldestLocked() {
			return
		}
	}
	if !c.process.tryReserve(bytes) {
		return
	}
	entry := &cacheEntry{key: key, object: slices.Clone(object), bytes: bytes}
	element := c.lru.PushFront(entry)
	c.entries[key] = element
	c.used += bytes
}

func (c *SharedBlockCache) evictOldestLocked() bool {
	element := c.lru.Back()
	if element == nil {
		return false
	}
	entry := element.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
	c.used -= entry.bytes
	c.process.release(entry.bytes)
	return true
}

func (c *SharedBlockCache) InvalidateRevision(file catalog.FileID, revision content.FileRevision) {
	identity := revisionCacheIdentity{file: file, revision: revision}
	c.mu.Lock()
	c.invalid[identity] = struct{}{}
	for key, element := range c.entries {
		if key.FileID == file && key.FileRevision == revision {
			entry := element.Value.(*cacheEntry)
			delete(c.entries, key)
			c.lru.Remove(element)
			c.used -= entry.bytes
			c.process.release(entry.bytes)
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
