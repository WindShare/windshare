package catalogflow

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
	c.mu.Unlock()
	for _, call := range calls {
		call.cancel()
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
		c.leaseClaimsByDirectory = nil
		c.residentEntries = 0
		c.usedBytes = 0
		c.inflightBytes = 0
		c.leaseClaimBytes = 0
		c.activeLeaseClaims = 0
	}
	c.mu.Unlock()
}
