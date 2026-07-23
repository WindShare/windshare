package catalogflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

var ErrClientClosed = errors.New("catalog client is closed")

const (
	DefaultClientCacheBytes      = uint64(16) << 20
	DefaultClientDirectories     = 4_096
	MaxConcurrentDirectoryLoads  = 4
	DirectoryFailureMemoryBytes  = 128
	CatalogLeaseClaimMemoryBytes = 64
	// MaxClientLeaseClaims keeps 16,384 claims * 64 bytes within 1 MiB even
	// when the caller configures an effectively unbounded cache.
	MaxClientLeaseClaims = 16_384
	// MaxDirectoryLeaseClaims keeps 256 claims * 64 bytes within 16 KiB so a
	// hot directory cannot monopolize the global claim budget.
	MaxDirectoryLeaseClaims = 256
)

type ClientConfig struct {
	ShareInstance           catalog.ShareInstance
	Transport               PageTransport
	Verifier                ObjectVerifier
	MaxPagesPerDirectory    uint32
	MaxObjectBytes          uint32
	MaxCacheBytes           uint64
	MaxDirectories          int
	MaxConcurrentLoads      int
	MaxLeaseClaims          int
	MaxDirectoryLeaseClaims int
	Now                     func() time.Time
}

type cachedResult struct {
	directory  catalog.DirectoryID
	snapshot   catalog.DirectorySnapshot
	failure    *DirectoryFailure
	retryAt    time.Time
	bytes      uint64
	persistent bool
	leases     int
	resident   bool
}

type acquireClaim struct {
	directory catalog.DirectoryID
	entry     *cachedResult
	released  bool
	accounted bool
}

type loadCall struct {
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	waiters           int
	persistentWaiters int
	acquireClaims     map[*acquireClaim]struct{}
	previousAttempt   catalog.ScanAttemptID
	previousResult    *cachedResult
	snapshot          catalog.DirectorySnapshot
	err               error
}

type Client struct {
	shareInstance  catalog.ShareInstance
	transport      PageTransport
	verifier       ObjectVerifier
	maxPages       uint32
	maxObjectBytes uint32
	maxCacheBytes  uint64
	maxDirectories int
	maxConcurrent  int
	now            func() time.Time

	mu                      sync.Mutex
	cache                   map[catalog.DirectoryID]*cachedResult
	inflight                map[catalog.DirectoryID]*loadCall
	residentEntries         int
	usedBytes               uint64
	inflightBytes           uint64
	leaseClaimBytes         uint64
	activeLeaseClaims       int
	leaseClaimsByDirectory  map[catalog.DirectoryID]int
	maxLeaseClaims          int
	maxDirectoryLeaseClaims int
	loads                   sync.WaitGroup
	stopped                 bool
	cleaned                 bool
}

func maxClientLeaseClaimsForCache(cacheBytes uint64) int {
	claimsByMemory := cacheBytes / CatalogLeaseClaimMemoryBytes
	cappedClaims := min(claimsByMemory, uint64(MaxClientLeaseClaims))
	return int(cappedClaims)
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.ShareInstance.IsZero() || config.Transport == nil || config.Verifier == nil {
		return nil, errors.New("catalog client requires share identity, transport, and verifier")
	}
	if config.MaxPagesPerDirectory == 0 {
		config.MaxPagesPerDirectory = catalog.MaxDirectoryEntries
	}
	if config.MaxPagesPerDirectory > catalog.MaxDirectoryEntries {
		return nil, errors.New("catalog client page budget exceeds the wire directory limit")
	}
	if config.MaxObjectBytes == 0 {
		config.MaxObjectBytes = catalog.MaxCatalogPageObjectBytes
	}
	if config.MaxObjectBytes > catalog.MaxCatalogPageObjectBytes {
		return nil, errors.New("catalog client object budget exceeds the wire object limit")
	}
	if config.MaxCacheBytes == 0 {
		config.MaxCacheBytes = DefaultClientCacheBytes
	}
	if config.MaxDirectories == 0 {
		config.MaxDirectories = DefaultClientDirectories
	}
	if config.MaxDirectories < 0 {
		return nil, errors.New("catalog client directory budget must be positive")
	}
	if config.MaxConcurrentLoads == 0 {
		config.MaxConcurrentLoads = MaxConcurrentDirectoryLoads
	}
	if config.MaxConcurrentLoads < 0 || config.MaxConcurrentLoads > MaxConcurrentDirectoryLoads {
		return nil, errors.New("catalog client concurrent-load budget exceeds the protocol-session scan limit")
	}
	maxClaimsByMemory := maxClientLeaseClaimsForCache(config.MaxCacheBytes)
	if config.MaxLeaseClaims == 0 {
		config.MaxLeaseClaims = maxClaimsByMemory
	}
	if config.MaxLeaseClaims < 0 || config.MaxLeaseClaims > maxClaimsByMemory {
		return nil, errors.New("catalog client lease-claim budget exceeds the cache memory limit")
	}
	if config.MaxDirectoryLeaseClaims == 0 {
		config.MaxDirectoryLeaseClaims = min(MaxDirectoryLeaseClaims, config.MaxLeaseClaims)
	}
	if config.MaxDirectoryLeaseClaims < 0 ||
		config.MaxDirectoryLeaseClaims > MaxDirectoryLeaseClaims ||
		config.MaxDirectoryLeaseClaims > config.MaxLeaseClaims {
		return nil, errors.New("catalog client per-directory lease-claim budget exceeds the client limit")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Client{
		shareInstance: config.ShareInstance, transport: config.Transport, verifier: config.Verifier,
		maxPages: config.MaxPagesPerDirectory, maxObjectBytes: config.MaxObjectBytes,
		maxCacheBytes: config.MaxCacheBytes, maxDirectories: config.MaxDirectories,
		maxConcurrent: config.MaxConcurrentLoads, now: config.Now,
		cache:                   make(map[catalog.DirectoryID]*cachedResult),
		inflight:                make(map[catalog.DirectoryID]*loadCall),
		leaseClaimsByDirectory:  make(map[catalog.DirectoryID]int),
		maxLeaseClaims:          config.MaxLeaseClaims,
		maxDirectoryLeaseClaims: config.MaxDirectoryLeaseClaims,
	}, nil
}

func (c *Client) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return catalog.DirectorySnapshot{}, err
	}
	if directory.IsZero() {
		return catalog.DirectorySnapshot{}, ErrInvalidRequest
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return catalog.DirectorySnapshot{}, ErrClientClosed
	}
	snapshot, call, immediate, err := c.beginRequestLocked(ctx, directory, true, nil)
	c.mu.Unlock()
	if immediate {
		return snapshot, err
	}
	return c.awaitLoad(ctx, directory, call)
}

// AcquireDirectory pins the exact cached result delivered to this caller. The
// callback must be invoked even when err is a DirectoryFailure, and is safe to
// invoke more than once.
func (c *Client) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	claim := &acquireClaim{directory: directory}
	release := func() { c.releaseAcquireClaim(claim) }
	if err := ctx.Err(); err != nil {
		claim.released = true
		return catalog.DirectorySnapshot{}, release, err
	}
	if directory.IsZero() {
		claim.released = true
		return catalog.DirectorySnapshot{}, release, ErrInvalidRequest
	}
	c.mu.Lock()
	if c.stopped {
		claim.released = true
		c.mu.Unlock()
		return catalog.DirectorySnapshot{}, release, ErrClientClosed
	}
	if !c.reserveAcquireClaimLocked(claim) {
		claim.released = true
		c.mu.Unlock()
		return catalog.DirectorySnapshot{}, release, ErrClientBudget
	}
	snapshot, call, immediate, err := c.beginRequestLocked(ctx, directory, false, claim)
	if immediate && claim.entry == nil {
		c.releaseAcquireClaimLocked(claim)
	}
	c.mu.Unlock()
	if immediate {
		return snapshot, release, err
	}
	snapshot, err = c.awaitAcquire(ctx, directory, call, claim)
	if claim.entry == nil {
		c.releaseAcquireClaim(claim)
	}
	return snapshot, release, err
}

func (c *Client) reserveAcquireClaimLocked(claim *acquireClaim) bool {
	if c.activeLeaseClaims >= c.maxLeaseClaims ||
		c.leaseClaimsByDirectory[claim.directory] >= c.maxDirectoryLeaseClaims ||
		CatalogLeaseClaimMemoryBytes > c.availableBytesLocked() {
		return false
	}
	claim.accounted = true
	c.activeLeaseClaims++
	c.leaseClaimsByDirectory[claim.directory]++
	c.leaseClaimBytes += CatalogLeaseClaimMemoryBytes
	return true
}

func (c *Client) beginRequestLocked(
	ctx context.Context,
	directory catalog.DirectoryID,
	persistent bool,
	claim *acquireClaim,
) (catalog.DirectorySnapshot, *loadCall, bool, error) {
	current := c.cache[directory]
	var previousAttempt catalog.ScanAttemptID
	if current != nil {
		if current.failure == nil || !current.failure.Retryable || c.now().Before(current.retryAt) {
			if persistent {
				current.persistent = true
			} else {
				current.leases++
				claim.entry = current
			}
			return current.snapshot, nil, true, current.loadError()
		}
		previousAttempt = current.failure.AttemptID
	}
	call := c.inflight[directory]
	if call == nil {
		if len(c.inflight) >= c.maxConcurrent {
			return catalog.DirectorySnapshot{}, nil, true, ErrClientBudget
		}
		callContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &loadCall{
			ctx: callContext, cancel: cancel, done: make(chan struct{}),
			previousAttempt: previousAttempt, previousResult: current,
			acquireClaims: make(map[*acquireClaim]struct{}),
		}
		c.inflight[directory] = call
		// Admission and accounting share c.mu so Close can never race Wait with
		// a late Add. The goroutine therefore belongs to the client, not its waiter.
		c.loads.Add(1)
		go c.runLoad(directory, call)
	}
	call.waiters++
	if persistent {
		call.persistentWaiters++
	} else {
		call.acquireClaims[claim] = struct{}{}
	}
	return catalog.DirectorySnapshot{}, call, false, nil
}

func (r *cachedResult) loadError() error {
	if r.failure == nil {
		return nil
	}
	return *r.failure
}

func (c *Client) awaitLoad(ctx context.Context, directory catalog.DirectoryID, call *loadCall) (catalog.DirectorySnapshot, error) {
	select {
	case <-call.done:
		return call.snapshot, call.err
	case <-ctx.Done():
		c.mu.Lock()
		if c.inflight[directory] != call {
			c.mu.Unlock()
			return call.snapshot, call.err
		}
		call.waiters--
		call.persistentWaiters--
		if call.waiters == 0 {
			call.cancel()
		}
		c.mu.Unlock()
		return catalog.DirectorySnapshot{}, ctx.Err()
	}
}

func (c *Client) awaitAcquire(
	ctx context.Context,
	directory catalog.DirectoryID,
	call *loadCall,
	claim *acquireClaim,
) (catalog.DirectorySnapshot, error) {
	select {
	case <-call.done:
		return call.snapshot, call.err
	case <-ctx.Done():
		c.mu.Lock()
		if c.inflight[directory] == call {
			delete(call.acquireClaims, claim)
			call.waiters--
			c.releaseAcquireClaimLocked(claim)
			if call.waiters == 0 {
				call.cancel()
			}
		} else {
			c.releaseAcquireClaimLocked(claim)
		}
		c.mu.Unlock()
		return catalog.DirectorySnapshot{}, ctx.Err()
	}
}

func (c *Client) runLoad(directory catalog.DirectoryID, call *loadCall) {
	defer c.loads.Done()
	snapshot, bytesUsed, err := c.fetchGeneration(call.ctx, directory)
	c.mu.Lock()
	c.inflightBytes -= bytesUsed
	if c.stopped {
		snapshot, err = catalog.DirectorySnapshot{}, ErrClientClosed
	} else {
		if call.waiters == 0 {
			err = context.Canceled
		}
		err = c.resolveRetryLocked(call.previousAttempt, err)
		err = c.cacheLoadResultLocked(directory, call, snapshot, bytesUsed, err)
	}
	delete(c.inflight, directory)
	call.snapshot, call.err = snapshot, err
	close(call.done)
	call.cancel()
	c.mu.Unlock()
}

func (c *Client) resolveRetryLocked(
	previousAttempt catalog.ScanAttemptID,
	loadErr error,
) error {
	if previousAttempt.IsZero() {
		return loadErr
	}
	var failure DirectoryFailure
	if errors.As(loadErr, &failure) && failure.AttemptID == previousAttempt {
		return ErrPageConflict
	}
	return loadErr
}

func (c *Client) cacheLoadResultLocked(
	directory catalog.DirectoryID,
	call *loadCall,
	snapshot catalog.DirectorySnapshot,
	bytesUsed uint64,
	loadErr error,
) error {
	if loadErr == nil {
		return c.cacheResultLocked(directory, call, snapshot, nil, time.Time{}, bytesUsed)
	}
	var failure DirectoryFailure
	if !errors.As(loadErr, &failure) {
		return loadErr
	}
	retryAt := time.Time{}
	if failure.Retryable {
		retryAt = c.now().Add(failure.RetryAfter)
	}
	if err := c.cacheResultLocked(
		directory, call, catalog.DirectorySnapshot{}, &failure, retryAt, DirectoryFailureMemoryBytes,
	); err != nil {
		return err
	}
	return failure
}

func (c *Client) cacheResultLocked(
	directory catalog.DirectoryID,
	call *loadCall,
	snapshot catalog.DirectorySnapshot,
	failure *DirectoryFailure,
	retryAt time.Time,
	bytesUsed uint64,
) error {
	previous := call.previousResult
	previousCurrent := previous != nil && c.cache[directory] == previous && previous.resident
	reclaimPrevious := previousCurrent && previous.leases == 0
	projectedEntries := c.residentEntries + 1
	projectedBytes := c.usedBytes + c.leaseClaimBytes
	if reclaimPrevious {
		projectedEntries--
		projectedBytes -= previous.bytes
	}
	if projectedEntries > c.maxDirectories || projectedBytes > c.maxCacheBytes ||
		bytesUsed > c.maxCacheBytes-projectedBytes {
		return ErrClientBudget
	}
	entry := &cachedResult{
		directory: directory, snapshot: snapshot, failure: failure, retryAt: retryAt,
		bytes: bytesUsed, persistent: call.persistentWaiters > 0, resident: true,
	}
	if previousCurrent && previous.persistent {
		entry.persistent = true
	}
	for claim := range call.acquireClaims {
		if claim.released {
			continue
		}
		claim.entry = entry
		entry.leases++
	}
	c.cache[directory] = entry
	c.usedBytes += bytesUsed
	c.residentEntries++
	if previousCurrent {
		previous.persistent = false
		c.maybeEvictResultLocked(previous)
	}
	return nil
}

func (c *Client) releaseAcquireClaim(claim *acquireClaim) {
	c.mu.Lock()
	c.releaseAcquireClaimLocked(claim)
	c.mu.Unlock()
}

func (c *Client) releaseAcquireClaimLocked(claim *acquireClaim) {
	if claim.released {
		return
	}
	claim.released = true
	if c.cleaned {
		// Close already detached the client's cache/accounting graph. The caller's
		// release remains safe and sheds its last entry reference without touching
		// reset counters.
		claim.accounted = false
		claim.entry = nil
		return
	}
	if claim.accounted {
		claim.accounted = false
		c.activeLeaseClaims--
		c.leaseClaimsByDirectory[claim.directory]--
		if c.leaseClaimsByDirectory[claim.directory] == 0 {
			delete(c.leaseClaimsByDirectory, claim.directory)
		}
		c.leaseClaimBytes -= CatalogLeaseClaimMemoryBytes
	}
	if claim.entry == nil {
		return
	}
	claim.entry.leases--
	c.maybeEvictResultLocked(claim.entry)
}

func (c *Client) maybeEvictResultLocked(entry *cachedResult) {
	if !entry.resident || entry.persistent || entry.leases != 0 {
		return
	}
	// Authenticated directory failures are session authority, not disposable
	// job data. Keeping the current failure preserves permanent failure reuse and
	// prevents retryable attempts from bypassing their authenticated cooldown.
	if c.cache[entry.directory] == entry && entry.failure != nil {
		return
	}
	if c.cache[entry.directory] == entry {
		delete(c.cache, entry.directory)
	}
	entry.resident = false
	c.usedBytes -= entry.bytes
	c.residentEntries--
}

func (c *Client) availableBytesLocked() uint64 {
	if c.usedBytes > c.maxCacheBytes || c.leaseClaimBytes > c.maxCacheBytes-c.usedBytes {
		return 0
	}
	retained := c.usedBytes + c.leaseClaimBytes
	if c.inflightBytes > c.maxCacheBytes-retained {
		return 0
	}
	return c.maxCacheBytes - retained - c.inflightBytes
}

func (c *Client) fetchGeneration(ctx context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, uint64, error) {
	assembler, err := NewAssembler(c.shareInstance, directory, c.maxPages)
	if err != nil {
		return catalog.DirectorySnapshot{}, 0, err
	}
	var bytesUsed uint64
	for {
		request, requestErr := assembler.NextRequest()
		if requestErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, requestErr
		}
		objectBytes, fetchErr := c.transport.FetchPage(ctx, request)
		if fetchErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, fetchErr
		}
		if len(objectBytes) == 0 || uint64(len(objectBytes)) > uint64(c.maxObjectBytes) {
			return catalog.DirectorySnapshot{}, bytesUsed, fmt.Errorf("%w: catalog object length %d", ErrClientBudget, len(objectBytes))
		}
		objectCharge := uint64(len(objectBytes))
		if reserveErr := c.reserveInflight(objectCharge); reserveErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, ErrClientBudget
		}
		bytesUsed += objectCharge
		verified, verifyErr := c.verifier.Verify(ctx, c.shareInstance, request, objectBytes)
		if verifyErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, fmt.Errorf("verify catalog object: %w", verifyErr)
		}
		if identityErr := validateVerifiedResponse(c.shareInstance, request, verified); identityErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, identityErr
		}
		if verified.Failure == nil {
			pageCharge := verified.Page.EstimatedMemoryBytes()
			if reserveErr := c.reserveInflight(pageCharge); reserveErr != nil {
				return catalog.DirectorySnapshot{}, bytesUsed, ErrClientBudget
			}
			bytesUsed += pageCharge
		}
		status, acceptErr := assembler.Accept(verified)
		if acceptErr != nil {
			return catalog.DirectorySnapshot{}, bytesUsed, acceptErr
		}
		if status == GenerationCommitted {
			snapshot, ok := assembler.Snapshot()
			if !ok {
				return catalog.DirectorySnapshot{}, bytesUsed, errors.New("catalog assembler reported commit without a snapshot")
			}
			return snapshot, bytesUsed, nil
		}
	}
}

func (c *Client) reserveInflight(bytes uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes > c.availableBytesLocked() {
		return ErrClientBudget
	}
	c.inflightBytes += bytes
	return nil
}

func validateVerifiedResponse(
	instance catalog.ShareInstance,
	request ListRequest,
	verified VerifiedObject,
) error {
	if verified.Failure != nil {
		if !verified.Page.ShareInstance().IsZero() {
			return ErrUnverifiedObject
		}
		failure := *verified.Failure
		if failure.ShareInstance != instance || failure.DirectoryID != request.directory ||
			request.pageIndex != 0 || request.generation != nil {
			return ErrResponseIdentity
		}
		return nil
	}
	page := verified.Page
	if page.ShareInstance().IsZero() {
		return ErrUnverifiedObject
	}
	if page.ShareInstance() != instance || page.DirectoryID() != request.directory || page.PageIndex() != request.pageIndex {
		return ErrResponseIdentity
	}
	if request.generation != nil && page.Generation() != *request.generation {
		return ErrResponseIdentity
	}
	return nil
}

func (c *Client) Snapshot(directory catalog.DirectoryID) (catalog.DirectorySnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.cache[directory]
	if !ok || cached.failure != nil {
		return catalog.DirectorySnapshot{}, false
	}
	return cached.snapshot, true
}

func (c *Client) ReleaseDirectory(directory catalog.DirectoryID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := c.cache[directory]
	if cached == nil || !cached.persistent {
		return false
	}
	cached.persistent = false
	if cached.failure != nil && cached.leases == 0 {
		delete(c.cache, directory)
		cached.resident = false
		c.usedBytes -= cached.bytes
		c.residentEntries--
		return true
	}
	c.maybeEvictResultLocked(cached)
	return true
}

func (c *Client) CachedBytes() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes + c.leaseClaimBytes
}
