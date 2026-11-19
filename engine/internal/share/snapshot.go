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

func (c *Controller) bindRelayStatus(read func() relayset.SenderStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.relayStatus, c.hasRelayStatus = read, true
}

// RelayStatus reads the relay owner independently of observation delivery.
func (c *Controller) RelayStatus() (relayset.SenderStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.relayStatus != nil {
		return c.relayStatus(), true
	}
	return c.finalRelayStatus, c.hasRelayStatus
}
