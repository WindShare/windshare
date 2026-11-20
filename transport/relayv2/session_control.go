package relayv2

import (
	"context"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var closedCapacityClearance = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// CapacityWait retains pressure briefly across local retirement so a completed
// handshake timeout can still be classified by its recovery owner.
func (c *Channel) CapacityWait() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	return max(0, c.capacityUntil[0].Sub(now), c.capacityUntil[1].Sub(now))
}

func (c *Channel) CapacityCleared() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacityCleared
}

func (c *Channel) CapacityClearance() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacityCleared {
		return closedCapacityClearance
	}
	if c.capacityClearance == nil {
		c.capacityClearance = make(chan struct{})
	}
	return c.capacityClearance
}

func (l *link) observeCapacityWait(wait v2.CapacityWait) {
	l.channelMu.Lock()
	channel := l.channels[wait.RelaySessionID]
	l.channelMu.Unlock()
	if channel == nil {
		return
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if wait.Milliseconds > 0 {
		channel.capacityUntil[wait.Direction] = time.Now().Add(time.Duration(wait.Milliseconds) * time.Millisecond)
		channel.capacityCleared = false
		return
	}
	if channel.capacityUntil[wait.Direction].IsZero() {
		return
	}
	channel.capacityUntil[wait.Direction] = time.Time{}
	now := time.Now()
	// Fetching a block uses both directions. A release on one direction must not
	// supersede live congestion advice still active on the other direction.
	if max(time.Duration(0), channel.capacityUntil[0].Sub(now), channel.capacityUntil[1].Sub(now)) > 0 || channel.capacityCleared {
		return
	}
	channel.capacityCleared = true
	if channel.capacityClearance != nil {
		close(channel.capacityClearance)
		channel.capacityClearance = nil
	}
}

type sendWindow struct{ frames, bytes int }

func (l *link) sendWindowLocked(id v2.RelaySessionID) *sendWindow {
	window := l.windows[id]
	if window == nil {
		window = &sendWindow{}
		l.windows[id] = window
	}
	return window
}

func (l *link) replenishCredit(credit v2.SessionCredit) bool {
	// The initial grant admits sender-side metadata before the first opaque
	// frame; neither endpoint may invent startup allowance locally.
	channel, _ := l.channel(credit.RelaySessionID)
	if channel == nil {
		return false
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	l.channelMu.Lock()
	defer l.channelMu.Unlock()
	// Credit can cross local retirement. It must never recreate a channel or
	// its budget after the authoritative owner has released it.
	if l.channels[credit.RelaySessionID] != channel || channel.state != framechannel.Open {
		return true
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	window := l.sendWindowLocked(credit.RelaySessionID)
	if int(credit.Frames) > v2.SenderWindowFrames-window.frames ||
		int(credit.Bytes) > v2.SenderWindowBytes-window.bytes {
		return false
	}
	window.frames += int(credit.Frames)
	window.bytes += int(credit.Bytes)
	select {
	case l.writeWake <- struct{}{}:
	default:
	}
	return true
}

// ConfirmAdmission retains a provisional relay session only after its application
// owner has authenticated the E2E handshake. It is sender-only and carries no
// opaque payload, so admission cannot be inferred from arbitrary receiver bytes.
func (c *Channel) ConfirmAdmission(ctx context.Context) error {
	if c.link.fixed {
		return framechannel.RejectSend(ErrProtocol)
	}
	encoded, err := (v2.SessionAdmitted{RelaySessionID: c.id}).MarshalBinary()
	if err != nil {
		return framechannel.RejectSend(err)
	}
	return c.link.enqueue(ctx, c, &sendRequest{
		kind: sendSessionAdmission, data: encoded, receipt: make(chan error, 1),
		channelID: c.id, operationID: c.link.nextOperationID(),
	})
}
