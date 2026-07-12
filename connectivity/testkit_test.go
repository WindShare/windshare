package connectivity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/session"
)

type observedFrame struct {
	frame    session.Frame
	terminal bool
}

type fakePeerChannel struct {
	mu         sync.Mutex
	state      session.ChannelState
	reason     error
	recv       chan session.Frame
	sent       chan observedFrame
	opened     chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	sendPermit <-chan struct{}
}

func newFakePeerChannel() *fakePeerChannel {
	opened := make(chan struct{})
	close(opened)
	permit := make(chan struct{})
	close(permit)
	return &fakePeerChannel{
		state:      session.Open,
		recv:       make(chan session.Frame, 32),
		sent:       make(chan observedFrame, 32),
		opened:     opened,
		done:       make(chan struct{}),
		sendPermit: permit,
	}
}

func (c *fakePeerChannel) Opened() <-chan struct{} { return c.opened }

func (c *fakePeerChannel) Done() <-chan struct{} { return c.done }

func (c *fakePeerChannel) Recv() <-chan session.Frame { return c.recv }

func (c *fakePeerChannel) State() session.ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *fakePeerChannel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

func (c *fakePeerChannel) Send(ctx context.Context, frame session.Frame) error {
	return c.send(ctx, frame, false)
}

func (c *fakePeerChannel) SendTerminal(ctx context.Context, frame session.Frame) error {
	if err := c.send(ctx, frame, true); err != nil {
		return err
	}
	return c.Close()
}

func (c *fakePeerChannel) send(ctx context.Context, frame session.Frame, terminal bool) error {
	if len(frame) == 0 || len(frame) > session.MaxFrameSize {
		return errors.New("fake channel: invalid frame")
	}
	if c.State() != session.Open {
		return errors.New("fake channel: closed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.sendPermit:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.sent <- observedFrame{frame: append(session.Frame(nil), frame...), terminal: terminal}:
		return nil
	}
}

func (c *fakePeerChannel) Close() error {
	c.closeWithError(nil)
	return nil
}

func (c *fakePeerChannel) closeWithError(reason error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.state = session.Closed
		c.reason = reason
		close(c.recv)
		close(c.done)
		c.mu.Unlock()
	})
}

func (c *fakePeerChannel) deliver(frame session.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != session.Open {
		return errors.New("fake channel: closed")
	}
	c.recv <- append(session.Frame(nil), frame...)
	return nil
}

func (c *fakePeerChannel) blockSends() chan struct{} {
	gate := make(chan struct{})
	c.sendPermit = gate
	return gate
}

type inertSignaling struct{ id string }

func (s *inertSignaling) Send(ctx context.Context, _ Signal) error { return ctx.Err() }

func (s *inertSignaling) Receive(ctx context.Context) (Signal, error) {
	<-ctx.Done()
	return Signal{}, ctx.Err()
}

type answerFactoryFunc func(context.Context, Signaling) (PeerChannel, error)

func (f answerFactoryFunc) Answer(ctx context.Context, signaling Signaling) (PeerChannel, error) {
	return f(ctx, signaling)
}

type offerFactoryFunc func(context.Context, Signaling) (PeerChannel, error)

func (f offerFactoryFunc) Offer(ctx context.Context, signaling Signaling) (PeerChannel, error) {
	return f(ctx, signaling)
}

type recordingStore struct {
	block []byte
	err   error
	calls atomic.Int64
}

func (s *recordingStore) ReadBlock(index uint64) ([]byte, error) {
	if index != 0 {
		return nil, fmt.Errorf("index %d", index)
	}
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return append([]byte(nil), s.block...), nil
}

func (s *recordingStore) BlockCount() uint64 { return 1 }

type recordingSealer struct{ calls atomic.Int64 }

func (s *recordingSealer) Seal(index uint64, plaintext []byte) ([]byte, error) {
	s.calls.Add(1)
	return append([]byte{byte(index)}, plaintext...), nil
}
