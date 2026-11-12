package share

import "github.com/windshare/windshare/connectivity/relayset"

// Startup never waits and retains failure as well as successful first readiness.
func (c *Controller) Startup() (Ready, error, bool) {
	select {
	case <-c.readyDone:
		return cloneReady(c.ready), c.readyErr, true
	default:
		return Ready{}, nil, false
	}
}

func (c *Controller) bindAvailability(read func() relayset.SenderAvailability) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.availability, c.hasAvailability = read, true
}

// Availability reads the relay owner, not its lossy observation stream.
func (c *Controller) Availability() (relayset.SenderAvailability, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.availability != nil {
		return c.availability(), true
	}
	return c.finalAvailability, c.hasAvailability
}
