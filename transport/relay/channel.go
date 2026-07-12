package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

type ingressResult uint8

const (
	ingressAccepted ingressResult = iota
	ingressClosed
	ingressOverflow
)

// Channel materializes one relay session as a core/session.FrameChannel. Frame
// and signaling ingress are both session-scoped and bounded, so the shared
// sender WebSocket reader never waits for an application consumer.
type Channel struct {
	sid protocol.SessionID
	l   *link

	recvCh    chan session.Frame
	signalsCh chan Signal

	mu     sync.Mutex
	state  session.ChannelState
	reason error

	terminalMu sync.Mutex
}

func newChannel(sid protocol.SessionID, l *link) *Channel {
	c := &Channel{
		sid:       sid,
		l:         l,
		state:     session.Open,
		recvCh:    make(chan session.Frame, recvBufferFrames+recvTerminalReserve),
		signalsCh: make(chan Signal, signalBuffer),
	}
	if result := l.pump.OpenSession(sid); result != forward.Enqueued {
		c.shut(sessionEnqueueError(result))
	}
	return c
}

func (c *Channel) SessionID() protocol.SessionID { return c.sid }

func (c *Channel) Send(ctx context.Context, frame session.Frame) error {
	if err := validateOutboundFrame(frame); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.isOpen() {
		return ErrChannelClosed
	}
	wire := protocol.EncodeForwardFrame(c.sid, frame)
	if err := c.l.enqueueForward(ctx, c.sid, wire); err != nil {
		if errors.Is(err, ErrConnClosed) || errors.Is(err, ErrSessionTerminal) {
			return ErrChannelClosed
		}
		return err
	}
	return nil
}

// SendTerminal waits for the local WS writer to accept the terminal envelope.
// Only then does the Channel become Closed, so a concurrent Close cannot enqueue
// bye ahead of an already accepted data-plane terminal.
func (c *Channel) SendTerminal(ctx context.Context, frame session.Frame) error {
	if err := validateOutboundFrame(frame); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if !c.isOpen() {
		return ErrChannelClosed
	}
	wire := protocol.EncodeTerminalForwardFrame(c.sid, frame)
	err := c.l.deliverTerminal(ctx, c.sid, outItem{binary: true, data: wire})
	// A terminal attempt owns the lifecycle once validation succeeds. The pump
	// may still drain an accepted terminal after caller cancellation; closing the
	// local streams prevents an unusable terminal queue from appearing Open.
	c.shut(err)
	return err
}

// SendSignal sends one negotiation message on this session's control lane.
// Scoping the operation to Channel removes the ambiguous connection-level API
// that accepted an unrelated session ID from its caller.
func (c *Channel) SendSignal(ctx context.Context, kind string, payload json.RawMessage) error {
	wire, err := protocol.Encode(protocol.NewSignal(c.sid.String(), kind, payload))
	if err != nil {
		return err
	}
	if !c.isOpen() {
		return ErrChannelClosed
	}
	if err := c.l.enqueueSessionControl(ctx, c.sid, outItem{data: wire}); err != nil {
		if errors.Is(err, ErrConnClosed) || errors.Is(err, ErrSessionTerminal) {
			return ErrChannelClosed
		}
		return err
	}
	return nil
}

func validateOutboundFrame(frame session.Frame) error {
	if len(frame) == 0 {
		return errors.New("relay: refusing to send empty frame")
	}
	if len(frame) > session.MaxFrameSize {
		return fmt.Errorf("relay: frame size %d exceeds MaxFrameSize %d", len(frame), session.MaxFrameSize)
	}
	return nil
}

func (c *Channel) Recv() <-chan session.Frame { return c.recvCh }

// Signals returns this session's bounded negotiation stream. It closes with the
// frame stream, and another session can neither fill nor consume it.
func (c *Channel) Signals() <-chan Signal { return c.signalsCh }

func (c *Channel) State() session.ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Channel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// Close is a normal terminal operation. It uses the same per-session terminal
// lane as SendTerminal, so queued data cannot overtake bye.
func (c *Channel) Close() error {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if !c.isOpen() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeHandshakeTimeout)
	defer cancel()
	wire := mustEncode(protocol.NewBye(c.sid.String()))
	err := c.l.deliverTerminal(ctx, c.sid, outItem{data: wire})
	c.shut(err)
	return err
}

func (c *Channel) isOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == session.Open
}

// shut is the sole non-terminal-frame closure boundary. State changes and both
// inbound channel closes share c.mu with delivery, eliminating send/close races
// without requiring the connection reader to wait for a consumer.
func (c *Channel) shut(reason error) {
	c.mu.Lock()
	closed := c.closeInboundLocked(reason)
	c.mu.Unlock()
	if closed {
		_ = c.l.pump.CloseSession(c.sid)
	}
}

func (c *Channel) closeInboundLocked(reason error) bool {
	if c.state == session.Closed {
		return false
	}
	c.state = session.Closed
	c.reason = reason
	close(c.recvCh)
	close(c.signalsCh)
	return true
}

// deliver performs a non-blocking ordinary-frame enqueue. The terminal reserve
// is never consumed here, so remote terminal delivery remains representable even
// when an application stops draining ordinary frames completely.
func (c *Channel) deliver(frame session.Frame) ingressResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != session.Open {
		return ingressClosed
	}
	if len(c.recvCh) >= recvBufferFrames {
		return ingressOverflow
	}
	c.recvCh <- append(session.Frame(nil), frame...)
	return ingressAccepted
}

func (c *Channel) deliverSignal(signal Signal) ingressResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != session.Open {
		return ingressClosed
	}
	if len(c.signalsCh) >= cap(c.signalsCh) {
		return ingressOverflow
	}
	signal.Payload = append(json.RawMessage(nil), signal.Payload...)
	c.signalsCh <- signal
	return ingressAccepted
}

// deliverTerminal atomically appends the final frame and closes both inbound
// streams. Closing a buffered channel preserves the final frame before callers
// observe Recv closure, while the reserved slot makes this path non-blocking.
func (c *Channel) deliverTerminal(frame session.Frame) ingressResult {
	c.mu.Lock()
	if c.state != session.Open {
		c.mu.Unlock()
		return ingressClosed
	}
	if len(c.recvCh) >= cap(c.recvCh) {
		c.mu.Unlock()
		return ingressOverflow
	}
	c.recvCh <- append(session.Frame(nil), frame...)
	closed := c.closeInboundLocked(nil)
	c.mu.Unlock()
	if closed {
		_ = c.l.pump.CloseSession(c.sid)
	}
	return ingressAccepted
}
