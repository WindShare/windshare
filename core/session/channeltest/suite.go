// Package channeltest defines the transport-neutral FrameChannel behavior matrix.
// Transport packages provide a factory that exposes peer-side controls without
// leaking their implementation details into the session module.
package channeltest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
)

const (
	operationTimeout   = 2 * time.Second
	blockedObservation = 50 * time.Millisecond
)

// SentFrame is one frame observed after the adapter accepted it for transport.
// Terminal remains explicit because terminal delivery has stronger completion
// and lifecycle semantics than an ordinary frame.
type SentFrame struct {
	Frame    session.Frame
	Terminal bool
}

// Fixture projects the peer side of one freshly opened adapter. The callbacks
// let the suite drive remote events while keeping transport construction,
// framing, and saturation mechanics inside the implementing package.
type Fixture struct {
	Channel session.FrameChannel

	ReceiveSent     func(context.Context) (SentFrame, error)
	Deliver         func(session.Frame) error
	DeliverTerminal func(session.Frame) error
	RemoteClose     func() error

	// SaturateSends must return only after the next valid Send will wait for
	// capacity. ReleaseSends must be idempotent so cleanup can always call it.
	SaturateSends func(testing.TB)
	ReleaseSends  func()
	Cleanup       func()
}

// Factory returns an independent open fixture for each conformance case.
type Factory func(testing.TB) Fixture

// Run executes the named behavior matrix shared by relay and future WebRTC
// adapters. Subtest names are stable so the TypeScript implementation can mirror
// the same contract even though its test code cannot import this Go package.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("channeltest: factory must not be nil")
	}

	t.Run("state-and-frame-bounds", func(t *testing.T) {
		fixture := openFixture(t, factory)
		if got := fixture.Channel.State(); got != session.Open {
			t.Fatalf("initial state = %v, want Open", got)
		}

		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := fixture.Channel.Send(canceled, session.Frame{1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Send with canceled context = %v, want context.Canceled", err)
		}
		if err := fixture.Channel.Send(context.Background(), nil); err == nil {
			t.Fatal("Send accepted an empty frame")
		}
		if err := fixture.Channel.SendTerminal(context.Background(), nil); err == nil {
			t.Fatal("SendTerminal accepted an empty frame")
		}
		oversize := make(session.Frame, session.MaxFrameSize+1)
		if err := fixture.Channel.Send(context.Background(), oversize); err == nil {
			t.Fatal("Send accepted an oversized frame")
		}
		if err := fixture.Channel.SendTerminal(context.Background(), oversize); err == nil {
			t.Fatal("SendTerminal accepted an oversized frame")
		}
		if got := fixture.Channel.State(); got != session.Open {
			t.Fatalf("invalid input changed state to %v", got)
		}

		want := patternedFrame(0x31, session.MaxFrameSize)
		if err := fixture.Channel.Send(context.Background(), want); err != nil {
			t.Fatalf("Send maximum frame: %v", err)
		}
		got := receiveSent(t, fixture)
		if got.Terminal || !bytes.Equal(got.Frame, want) {
			t.Fatalf("maximum frame mismatch: terminal=%v bytes=%d", got.Terminal, len(got.Frame))
		}
	})

	t.Run("payload-ownership", func(t *testing.T) {
		fixture := openFixture(t, factory)

		// Frame buffers remain caller-owned. Adapters must snapshot accepted
		// payloads so callers can immediately reuse pooled buffers without racing
		// transport writers or changing bytes already queued for delivery.
		outbound := patternedFrame(0x35, 257)
		wantOutbound := append(session.Frame(nil), outbound...)
		if err := fixture.Channel.Send(context.Background(), outbound); err != nil {
			t.Fatalf("Send ownership frame: %v", err)
		}
		mutate(outbound)
		gotOutbound := receiveSent(t, fixture)
		if gotOutbound.Terminal || !bytes.Equal(gotOutbound.Frame, wantOutbound) {
			t.Fatal("outbound frame changed after caller reused its buffer")
		}

		inbound := patternedFrame(0x36, 257)
		wantInbound := append(session.Frame(nil), inbound...)
		if err := fixture.Deliver(inbound); err != nil {
			t.Fatalf("deliver ownership frame: %v", err)
		}
		mutate(inbound)
		if got := receiveFrame(t, fixture.Channel); !bytes.Equal(got, wantInbound) {
			t.Fatal("inbound frame changed after peer reused its buffer")
		}

		terminal := patternedFrame(0x37, 64)
		wantTerminal := append(session.Frame(nil), terminal...)
		if err := fixture.DeliverTerminal(terminal); err != nil {
			t.Fatalf("deliver ownership terminal: %v", err)
		}
		mutate(terminal)
		if got := receiveFrame(t, fixture.Channel); !bytes.Equal(got, wantTerminal) {
			t.Fatal("inbound terminal changed after peer reused its buffer")
		}
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
	})

	t.Run("backpressure-cancellation", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- fixture.Channel.Send(ctx, session.Frame{0x42})
		}()
		select {
		case err := <-result:
			t.Fatalf("saturated Send returned before cancellation: %v", err)
		case <-time.After(blockedObservation):
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled saturated Send = %v, want context.Canceled", err)
			}
		case <-time.After(operationTimeout):
			t.Fatal("canceled saturated Send did not wake")
		}
		fixture.ReleaseSends()
	})

	t.Run("backpressure-recovery", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)
		want := patternedFrame(0x47, 257)
		result := make(chan error, 1)
		go func() { result <- fixture.Channel.Send(context.Background(), want) }()
		select {
		case err := <-result:
			t.Fatalf("saturated Send returned before release: %v", err)
		case <-time.After(blockedObservation):
		}
		fixture.ReleaseSends()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("Send after capacity recovery: %v", err)
			}
		case <-time.After(operationTimeout):
			t.Fatal("Send did not resume after capacity recovery")
		}
		receiveSentFrame(t, fixture, want)
	})

	t.Run("backpressure-remote-close", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)
		result := make(chan error, 1)
		go func() { result <- fixture.Channel.Send(context.Background(), session.Frame{0x48}) }()
		select {
		case err := <-result:
			t.Fatalf("saturated Send returned before remote close: %v", err)
		case <-time.After(blockedObservation):
		}
		if err := fixture.RemoteClose(); err != nil {
			t.Fatalf("remote close while Send blocked: %v", err)
		}
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("blocked Send succeeded after remote close")
			}
		case <-time.After(operationTimeout):
			t.Fatal("remote close did not wake blocked Send")
		}
		fixture.ReleaseSends()
	})

	t.Run("outbound-terminal", func(t *testing.T) {
		fixture := openFixture(t, factory)
		want := patternedFrame(0x53, 64)
		result := make(chan error, 1)
		go func() { result <- fixture.Channel.SendTerminal(context.Background(), want) }()
		got := receiveSent(t, fixture)
		if !got.Terminal || !bytes.Equal(got.Frame, want) {
			t.Fatalf("terminal mismatch: terminal=%v bytes=%x", got.Terminal, got.Frame)
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("SendTerminal: %v", err)
			}
		case <-time.After(operationTimeout):
			t.Fatal("SendTerminal did not complete after peer delivery")
		}
		assertClosed(t, fixture.Channel)
		assertRecvClosed(t, fixture.Channel)
		if err := fixture.Channel.Send(context.Background(), session.Frame{1}); err == nil {
			t.Fatal("Send succeeded after terminal")
		}
		if err := fixture.Channel.SendTerminal(context.Background(), session.Frame{1}); err == nil {
			t.Fatal("second terminal succeeded")
		}
		if err := fixture.Channel.Close(); err != nil {
			t.Fatalf("Close after terminal: %v", err)
		}
	})

	t.Run("terminal-not-overtaken-by-close", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)
		want := patternedFrame(0x54, 64)
		terminalResult := make(chan error, 1)
		go func() { terminalResult <- fixture.Channel.SendTerminal(context.Background(), want) }()
		select {
		case err := <-terminalResult:
			t.Fatalf("terminal completed while transport was blocked: %v", err)
		case <-time.After(blockedObservation):
		}
		closeResult := make(chan error, 1)
		go func() { closeResult <- fixture.Channel.Close() }()
		select {
		case err := <-closeResult:
			t.Fatalf("Close overtook accepted terminal: %v", err)
		case <-time.After(blockedObservation):
		}

		fixture.ReleaseSends()
		receiveSentTerminal(t, fixture, want)
		select {
		case err := <-terminalResult:
			if err != nil {
				t.Fatalf("SendTerminal after release: %v", err)
			}
		case <-time.After(operationTimeout):
			t.Fatal("SendTerminal did not complete after release")
		}
		select {
		case err := <-closeResult:
			if err != nil {
				t.Fatalf("Close after terminal: %v", err)
			}
		case <-time.After(operationTimeout):
			t.Fatal("Close remained blocked after terminal completion")
		}
		assertClosed(t, fixture.Channel)
	})

	t.Run("inbound-terminal-before-close", func(t *testing.T) {
		fixture := openFixture(t, factory)
		ordinary := patternedFrame(0x61, session.MaxFrameSize)
		terminal := patternedFrame(0x62, 64)
		if err := fixture.Deliver(ordinary); err != nil {
			t.Fatalf("deliver ordinary frame: %v", err)
		}
		if err := fixture.DeliverTerminal(terminal); err != nil {
			t.Fatalf("deliver terminal frame: %v", err)
		}
		// Late peer traffic may be reported or silently discarded, but it must
		// never revive the stream or appear after the terminal.
		_ = fixture.Deliver(session.Frame{0xff})

		if got := receiveFrame(t, fixture.Channel); !bytes.Equal(got, ordinary) {
			t.Fatal("ordinary frame changed before delivery")
		}
		if got := receiveFrame(t, fixture.Channel); !bytes.Equal(got, terminal) {
			t.Fatal("terminal was not the final received frame")
		}
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
	})

	t.Run("close-idempotence", func(t *testing.T) {
		fixture := openFixture(t, factory)
		if err := fixture.Channel.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := fixture.Channel.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		assertClosed(t, fixture.Channel)
		assertRecvClosed(t, fixture.Channel)
		if err := fixture.Channel.Send(context.Background(), session.Frame{1}); err == nil {
			t.Fatal("Send succeeded after Close")
		}
	})

	t.Run("remote-close-and-late-traffic", func(t *testing.T) {
		fixture := openFixture(t, factory)
		if err := fixture.RemoteClose(); err != nil {
			t.Fatalf("remote close: %v", err)
		}
		_ = fixture.Deliver(session.Frame{1})
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
		if err := fixture.Channel.Send(context.Background(), session.Frame{1}); err == nil {
			t.Fatal("Send succeeded after remote close")
		}
	})
}

func openFixture(t *testing.T, factory Factory) Fixture {
	t.Helper()
	fixture := factory(t)
	switch {
	case fixture.Channel == nil:
		t.Fatal("channeltest: fixture Channel must not be nil")
	case fixture.ReceiveSent == nil:
		t.Fatal("channeltest: fixture ReceiveSent must not be nil")
	case fixture.Deliver == nil:
		t.Fatal("channeltest: fixture Deliver must not be nil")
	case fixture.DeliverTerminal == nil:
		t.Fatal("channeltest: fixture DeliverTerminal must not be nil")
	case fixture.RemoteClose == nil:
		t.Fatal("channeltest: fixture RemoteClose must not be nil")
	case fixture.SaturateSends == nil:
		t.Fatal("channeltest: fixture SaturateSends must not be nil")
	case fixture.ReleaseSends == nil:
		t.Fatal("channeltest: fixture ReleaseSends must not be nil")
	case fixture.Cleanup == nil:
		t.Fatal("channeltest: fixture Cleanup must not be nil")
	}
	t.Cleanup(func() {
		fixture.ReleaseSends()
		fixture.Cleanup()
	})
	return fixture
}

func receiveSent(t *testing.T, fixture Fixture) SentFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	got, err := fixture.ReceiveSent(ctx)
	if err != nil {
		t.Fatalf("receive sent frame: %v", err)
	}
	return got
}

func receiveSentFrame(t *testing.T, fixture Fixture, want session.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for {
		got, err := fixture.ReceiveSent(ctx)
		if err != nil {
			t.Fatalf("receive recovered frame: %v", err)
		}
		if !got.Terminal && bytes.Equal(got.Frame, want) {
			return
		}
	}
}

func receiveSentTerminal(t *testing.T, fixture Fixture, want session.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for {
		got, err := fixture.ReceiveSent(ctx)
		if err != nil {
			t.Fatalf("receive terminal frame: %v", err)
		}
		if got.Terminal && bytes.Equal(got.Frame, want) {
			return
		}
	}
}

func receiveFrame(t *testing.T, channel session.FrameChannel) session.Frame {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		if !ok {
			t.Fatal("Recv closed before expected frame")
		}
		return frame
	case <-time.After(operationTimeout):
		t.Fatal("timeout waiting for received frame")
		return nil
	}
}

func assertRecvClosed(t *testing.T, channel session.FrameChannel) {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		if ok {
			t.Fatalf("Recv yielded frame after terminal close: %x", frame)
		}
	case <-time.After(operationTimeout):
		t.Fatal("Recv did not close")
	}
}

func assertClosed(t *testing.T, channel session.FrameChannel) {
	t.Helper()
	if got := channel.State(); got != session.Closed {
		t.Fatalf("state = %v, want Closed", got)
	}
}

func patternedFrame(marker byte, size int) session.Frame {
	frame := make(session.Frame, size)
	if size == 0 {
		return frame
	}
	frame[0] = marker
	for i := 1; i < len(frame); i++ {
		frame[i] = byte((i*31 + 17) % 251)
	}
	return frame
}

func mutate(frame session.Frame) {
	for i := range frame {
		frame[i] ^= 0xff
	}
}
