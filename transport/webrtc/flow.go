package webrtc

import "context"

func (c *Channel) acquireSendTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.lifecycle.stopSignal():
		return c.lifecycle.closedError()
	case <-c.sendTurn:
		return nil
	}
}

func (c *Channel) releaseSendTurn() {
	c.sendTurn <- struct{}{}
}

// waitForCapacity implements hysteresis rather than treating every callback as
// permission to send. A stale or coalesced callback only wakes the waiter; the
// authoritative buffered amount must cross the low-water boundary.
func (c *Channel) waitForCapacity(ctx context.Context, required terminalState) error {
	if err := c.lifecycle.requireSendState(required); err != nil {
		return err
	}
	amount := c.dc.BufferedAmount()
	if amount < c.flow.highWaterBytes {
		return nil
	}
	for amount > c.flow.lowWaterBytes {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.lifecycle.stopSignal():
			return c.lifecycle.closedError()
		case <-c.flowWake:
		case <-c.lifecycle.stateWakeSignal():
		}
		if err := c.lifecycle.requireSendState(required); err != nil {
			return err
		}
		amount = c.dc.BufferedAmount()
	}
	return c.lifecycle.requireSendState(required)
}

func (c *Channel) onBufferedAmountLow() {
	select {
	case c.flowWake <- struct{}{}:
	default:
	}
}
