package webrtc

import "context"

func (c *Channel) acquireSendAdmissionTurn(admission *sendAdmission) error {
	select {
	case <-admission.done:
		return c.lifecycle.sendAdmissionError(admission)
	case <-c.sendTurn:
		if err := c.lifecycle.sendAdmissionError(admission); err != nil {
			c.releaseSendTurn()
			return err
		}
		return nil
	}
}

func (c *Channel) acquireOwnedSendTurn(ctx context.Context) error {
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

// Capacity callbacks are hints only. Rechecking both the durable admission and
// buffered amount prevents a stale wake from reviving a canceled or retired send.
func (c *Channel) waitForSendAdmissionCapacity(admission *sendAdmission) error {
	if err := c.lifecycle.sendAdmissionError(admission); err != nil {
		return err
	}
	amount := c.dc.BufferedAmount()
	if amount < c.flow.highWaterBytes {
		return nil
	}
	for amount > c.flow.lowWaterBytes {
		select {
		case <-admission.done:
			return c.lifecycle.sendAdmissionError(admission)
		case <-c.flowWake:
		case <-c.lifecycle.stateWakeSignal():
		}
		if err := c.lifecycle.sendAdmissionError(admission); err != nil {
			return err
		}
		amount = c.dc.BufferedAmount()
	}
	return c.lifecycle.sendAdmissionError(admission)
}

// waitForOwnedCapacity is used only after terminal lifecycle ownership became
// irreversible. Every later cancellation or lifecycle error therefore remains
// an accepted send failure rather than being reclassified as pre-admission.
func (c *Channel) waitForOwnedCapacity(ctx context.Context, required terminalState) error {
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
