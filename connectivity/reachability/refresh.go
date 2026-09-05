package reachability

// RefreshUnavailable consumes a discovery-catalog change, not a retry timer.
// Only resources with live content demand and no successful lease are eligible;
// the next background maintenance pass remains bounded by normal gateway limits.
func (a *Authority) RefreshUnavailable() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.config.Now()
	for key, resource := range a.resources {
		if resource.gateway == nil && a.needed(key, now, false) {
			if resource.cancel != nil {
				resource.refreshPending = true
			} else {
				resource.attempted = false
				resource.nextGateway = 0
			}
		}
	}
	a.signal()
}
