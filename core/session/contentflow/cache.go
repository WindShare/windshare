package contentflow

import (
	"container/list"
	"context"
	"errors"
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
	ErrInvalidCacheKey = errors.New("block payload cache key is invalid")
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

// cachedBlock holds an immutable encoded block payload alongside the RecordID
// committed when that payload entered the cache. Storing payload as a string
// makes cache hits and inflight fan-out zero-copy while preventing in-package
// callers from mutating shared bytes.
type cachedBlock struct {
	payload  string
	recordID records.RecordID
}

type cacheEntry struct {
	key   BlockCacheKey
	block cachedBlock
	bytes uint64
}

type cacheCall struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	waiters     int
	completed   bool
	uncacheable bool
	block       cachedBlock
	err         error
}

type SharedBlockCache struct {
	share       catalog.ShareInstance
	maxBytes    uint64
	reservation cacheReservation

	mu            sync.Mutex
	loads         sync.WaitGroup
	closed        bool
	used          uint64
	entries       map[BlockCacheKey]*list.Element
	entriesByRev  map[revisionCacheIdentity]map[*list.Element]struct{}
	lru           *list.List
	inflight      map[BlockCacheKey]*cacheCall
	inflightByRev map[revisionCacheIdentity]map[BlockCacheKey]*cacheCall
	watches       map[revisionCacheIdentity]map[*revisionWatch]struct{}
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
		entries:       make(map[BlockCacheKey]*list.Element),
		entriesByRev:  make(map[revisionCacheIdentity]map[*list.Element]struct{}),
		lru:           list.New(),
		inflight:      make(map[BlockCacheKey]*cacheCall),
		inflightByRev: make(map[revisionCacheIdentity]map[BlockCacheKey]*cacheCall),
		watches:       make(map[revisionCacheIdentity]map[*revisionWatch]struct{}),
	}, nil
}

func (c *SharedBlockCache) Get(ctx context.Context, key BlockCacheKey, load func(context.Context) ([]byte, error)) ([]byte, error) {
	block, err := c.get(ctx, key, load)
	if err != nil {
		return nil, err
	}
	return []byte(block.payload), nil
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
	if element := c.entries[key]; element != nil {
		c.lru.MoveToFront(element)
		block := element.Value.(*cacheEntry).block
		c.mu.Unlock()
		return block, nil
	}
	call := c.inflight[key]
	if call == nil {
		callContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &cacheCall{ctx: callContext, cancel: cancel, done: make(chan struct{}), waiters: 1}
		c.addInflightLocked(key, call)
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
		return call.block, call.err
	case <-ctx.Done():
		c.mu.Lock()
		if c.inflight[key] == call && !call.completed {
			call.waiters--
			if call.waiters == 0 {
				c.removeInflightLocked(key)
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
			block.recordID = records.RecordIDFromObject(payload)
			block.payload = string(payload)
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
		c.removeInflightLocked(key)
	}
	// Invalidation removes the call before cancelling it; clean retirement marks
	// it uncacheable. Neither path may repopulate the cache with a dead revision.
	if err == nil && active && !call.uncacheable && call.waiters > 0 && !c.closed {
		c.storeLocked(key, block)
	}
	call.block, call.err = block, err
	close(call.done)
	call.cancel()
	c.mu.Unlock()
}

func (c *SharedBlockCache) addInflightLocked(key BlockCacheKey, call *cacheCall) {
	c.inflight[key] = call
	identity := revisionCacheIdentity{file: key.FileID, revision: key.FileRevision}
	calls := c.inflightByRev[identity]
	if calls == nil {
		calls = make(map[BlockCacheKey]*cacheCall)
		c.inflightByRev[identity] = calls
	}
	calls[key] = call
}

func (c *SharedBlockCache) removeInflightLocked(key BlockCacheKey) {
	delete(c.inflight, key)
	identity := revisionCacheIdentity{file: key.FileID, revision: key.FileRevision}
	if calls := c.inflightByRev[identity]; calls != nil {
		delete(calls, key)
		if len(calls) == 0 {
			delete(c.inflightByRev, identity)
		}
	}
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
	identity := revisionCacheIdentity{file: key.FileID, revision: key.FileRevision}
	entries := c.entriesByRev[identity]
	if entries == nil {
		entries = make(map[*list.Element]struct{})
		c.entriesByRev[identity] = entries
	}
	entries[element] = struct{}{}
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
	identity := revisionCacheIdentity{file: entry.key.FileID, revision: entry.key.FileRevision}
	if entries := c.entriesByRev[identity]; entries != nil {
		delete(entries, element)
		if len(entries) == 0 {
			delete(c.entriesByRev, identity)
		}
	}
	c.lru.Remove(element)
	c.used -= entry.bytes
	c.reservation.shrink(entry.bytes)
}

func (c *SharedBlockCache) evictRevisionEntriesLocked(identity revisionCacheIdentity) {
	for element := range c.entriesByRev[identity] {
		c.removeEntryLocked(element)
	}
}

// RetireRevision reclaims cached entries for a cleanly retired open-handle
// revision and prevents its remaining in-flight loads from caching, without
// aborting active sends as source drift.
func (c *SharedBlockCache) RetireRevision(file catalog.FileID, revision content.FileRevision) {
	identity := revisionCacheIdentity{file: file, revision: revision}
	c.mu.Lock()
	c.evictRevisionEntriesLocked(identity)
	for _, call := range c.inflightByRev[identity] {
		call.uncacheable = true
	}
	c.mu.Unlock()
}

// InvalidateRevision evicts cached entries, aborts in-flight loads, and cancels
// watches for a revoked open-handle revision. RevisionStore revokes leases and
// never reissues a random FileRevision, so the cache needs no tombstone set.
func (c *SharedBlockCache) InvalidateRevision(file catalog.FileID, revision content.FileRevision) {
	identity := revisionCacheIdentity{file: file, revision: revision}
	c.mu.Lock()
	c.evictRevisionEntriesLocked(identity)
	for key, call := range c.inflightByRev[identity] {
		c.removeInflightLocked(key)
		c.completeTerminatedCallLocked(call, content.ErrRevisionDrift)
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
			c.removeInflightLocked(key)
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
