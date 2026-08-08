package catalogflow

import "github.com/windshare/windshare/core/catalog"

// Stop synchronously freezes admission and cancels every owned load without
// joining callbacks. This split lets a verifier or transport callback request
// shutdown without waiting for the goroutine that is currently invoking it.
func (c *Client) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	calls := make([]*loadCall, 0, len(c.inflight))
	for _, call := range c.inflight {
		calls = append(calls, call)
	}
	cursorCancels := make([]func(), 0, len(c.cursorFetchCancels))
	for _, cancel := range c.cursorFetchCancels {
		cursorCancels = append(cursorCancels, cancel)
	}
	c.mu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
	for _, cancel := range cursorCancels {
		cancel()
	}
}

// Close is the owner-side join. Callbacks may invoke Stop, but must leave Close
// to their owner because joining from an owned callback would self-deadlock.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.Stop()
	c.loads.Wait()
	c.mu.Lock()
	if !c.cleaned {
		c.cleaned = true
		// Claims may outlive the client, so their entry pointers remain caller-owned.
		// Clearing only the client graph lets a later release become a safe no-op
		// against reset accounting while promptly dropping cache and crypto borrowers.
		c.transport = nil
		c.verifier = nil
		c.now = nil
		c.cache = nil
		c.inflight = nil
		c.cursorFetchCancels = nil
		c.leaseClaimsByDirectory = nil
		c.residentEntries = 0
		c.usedBytes = 0
		c.inflightBytes = 0
		c.leaseClaimBytes = 0
		c.activeLeaseClaims = 0
	}
	c.mu.Unlock()
}

func (result *cachedResult) loadError() error {
	if result.failure == nil {
		return nil
	}
	return *result.failure
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
