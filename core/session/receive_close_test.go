package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type closeBlockingChannel struct {
	started     chan struct{}
	recv        chan Frame
	closed      chan struct{}
	startedOnce sync.Once
	closeOnce   sync.Once
}

type requestObservingChannel struct {
	observed  chan struct{}
	recv      chan Frame
	closed    chan struct{}
	observe   sync.Once
	closeOnce sync.Once
}

func newRequestObservingChannel() *requestObservingChannel {
	return &requestObservingChannel{
		observed: make(chan struct{}),
		recv:     make(chan Frame),
		closed:   make(chan struct{}),
	}
}

func (c *requestObservingChannel) Send(ctx context.Context, _ Frame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrSessionClosed
	default:
		c.observe.Do(func() { close(c.observed) })
		return nil
	}
}

func (c *requestObservingChannel) SendTerminal(ctx context.Context, frame Frame) error {
	return c.Send(ctx, frame)
}

func (c *requestObservingChannel) Recv() <-chan Frame { return c.recv }

func (c *requestObservingChannel) State() ChannelState {
	select {
	case <-c.closed:
		return Closed
	default:
		return Open
	}
}

func (c *requestObservingChannel) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		close(c.recv)
	})
	return nil
}

func newCloseBlockingChannel() *closeBlockingChannel {
	return &closeBlockingChannel{
		started: make(chan struct{}),
		recv:    make(chan Frame),
		closed:  make(chan struct{}),
	}
}

func (c *closeBlockingChannel) Send(ctx context.Context, _ Frame) error {
	c.startedOnce.Do(func() { close(c.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrSessionClosed
	}
}

func (c *closeBlockingChannel) SendTerminal(ctx context.Context, frame Frame) error {
	return c.Send(ctx, frame)
}

func (c *closeBlockingChannel) Recv() <-chan Frame { return c.recv }

func (c *closeBlockingChannel) State() ChannelState {
	select {
	case <-c.closed:
		return Closed
	default:
		return Open
	}
}

func (c *closeBlockingChannel) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		close(c.recv)
	})
	return nil
}

func TestReceiveCloseInterruptsBlockedRequestSend(t *testing.T) {
	checkNoLeak(t)
	r := newRig(t, 1, 64, []uint64{0}, defaultOptions())
	ch := newCloseBlockingChannel()
	t.Cleanup(func() { _ = ch.Close() })
	if err := r.sess.AddChannel(ch); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	done := mustRun(r.sess, context.Background())

	select {
	case <-ch.started:
	case <-time.After(time.Second):
		t.Fatal("request send did not block")
	}
	if err := r.sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitErr(t, done, time.Second); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Run = %v, want ErrSessionClosed", err)
	}
}

func TestReceiveBlockedRequestSendDoesNotStallHealthySibling(t *testing.T) {
	checkNoLeak(t)
	r := newRig(t, 2*InFlightWindow, 64, allIndices(2*InFlightWindow), defaultOptions())
	blocked := newCloseBlockingChannel()
	healthy := newRequestObservingChannel()
	t.Cleanup(func() { _ = blocked.Close() })
	t.Cleanup(func() { _ = healthy.Close() })
	if err := r.sess.AddChannel(blocked); err != nil {
		t.Fatalf("AddChannel(blocked): %v", err)
	}
	if err := r.sess.AddChannel(healthy); err != nil {
		t.Fatalf("AddChannel(healthy): %v", err)
	}
	done := mustRun(r.sess, context.Background())
	defer func() {
		_ = r.sess.Close()
		_ = waitErr(t, done, time.Second)
	}()

	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("blocked channel did not receive a request")
	}
	select {
	case <-healthy.observed:
	case <-time.After(time.Second):
		t.Fatal("blocked channel stalled request scheduling for its healthy sibling")
	}
}

func TestReceiveBlockedRequestSendTimesOutAndReassigns(t *testing.T) {
	checkNoLeak(t)
	opts := defaultOptions()
	opts.RequestTimeout = 50 * time.Millisecond
	selected := allIndices(2 * InFlightWindow)
	r := newRig(t, len(selected), 64, selected, opts)
	blocked := newCloseBlockingChannel()
	t.Cleanup(func() { _ = blocked.Close() })
	if err := r.sess.AddChannel(blocked); err != nil {
		t.Fatalf("AddChannel(blocked): %v", err)
	}
	r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 2*time.Second); err != nil {
		t.Fatalf("Run after blocked-send reassignment: %v", err)
	}
	r.verify(selected)
}
