package channeltest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/channeltest"
)

var errReferenceClosed = errors.New("reference channel closed")

type referenceChannel struct {
	mu         sync.Mutex
	terminalMu sync.Mutex
	state      framechannel.ChannelState
	recv       chan framechannel.Frame
	sent       chan channeltest.SentFrame
	blocked    chan struct{}
	closed     chan struct{}
}

func newReferenceChannel() *referenceChannel {
	return &referenceChannel{
		state:  framechannel.Open,
		recv:   make(chan framechannel.Frame, 4),
		sent:   make(chan channeltest.SentFrame, 8),
		closed: make(chan struct{}),
	}
}

func (c *referenceChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	return c.send(ctx, frame, false)
}

func (c *referenceChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if err := c.send(ctx, frame, true); err != nil {
		return err
	}
	c.close()
	return nil
}

func (c *referenceChannel) send(ctx context.Context, frame framechannel.Frame, terminal bool) error {
	if len(frame) == 0 || len(frame) > framechannel.MaxFrameSize {
		return fmt.Errorf("reference: invalid frame length %d", len(frame))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.state != framechannel.Open {
		c.mu.Unlock()
		return errReferenceClosed
	}
	blocked := c.blocked
	c.mu.Unlock()
	if blocked != nil {
		select {
		case <-blocked:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return errReferenceClosed
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != framechannel.Open {
		return errReferenceClosed
	}
	select {
	case c.sent <- channeltest.SentFrame{Frame: append(framechannel.Frame(nil), frame...), Terminal: terminal}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *referenceChannel) Recv() <-chan framechannel.Frame { return c.recv }

func (c *referenceChannel) State() framechannel.ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *referenceChannel) Close() error {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	c.close()
	return nil
}

func (c *referenceChannel) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == framechannel.Closed {
		return
	}
	c.state = framechannel.Closed
	close(c.closed)
	close(c.recv)
}

func (c *referenceChannel) deliver(frame framechannel.Frame, terminal bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != framechannel.Open {
		return errReferenceClosed
	}
	if len(frame) == 0 || len(frame) > framechannel.MaxFrameSize {
		return fmt.Errorf("reference: invalid peer frame length %d", len(frame))
	}
	c.recv <- append(framechannel.Frame(nil), frame...)
	if terminal {
		c.state = framechannel.Closed
		close(c.closed)
		close(c.recv)
	}
	return nil
}

func (c *referenceChannel) saturate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked == nil {
		c.blocked = make(chan struct{})
	}
}

func (c *referenceChannel) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked != nil {
		close(c.blocked)
		c.blocked = nil
	}
}

func TestBehaviorMatrixAgainstReferenceChannel(t *testing.T) {
	channeltest.Run(t, func(testing.TB) channeltest.Fixture {
		channel := newReferenceChannel()
		return channeltest.Fixture{
			Channel: channel,
			ReceiveSent: func(ctx context.Context) (channeltest.SentFrame, error) {
				select {
				case sent := <-channel.sent:
					return sent, nil
				case <-ctx.Done():
					return channeltest.SentFrame{}, ctx.Err()
				}
			},
			Deliver: func(frame framechannel.Frame) error {
				return channel.deliver(frame, false)
			},
			DeliverTerminal: func(frame framechannel.Frame) error {
				return channel.deliver(frame, true)
			},
			RemoteClose: func() error {
				channel.close()
				return nil
			},
			SaturateSends: func(testing.TB) { channel.saturate() },
			ReleaseSends:  channel.release,
			Cleanup:       channel.close,
		}
	})
}
