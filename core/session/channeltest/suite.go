// Package channeltest defines the transport-neutral FrameChannel behavior matrix.
// Transport packages provide a factory that exposes peer-side controls without
// leaking their implementation details into the session module.
package channeltest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
)

const (
	operationTimeout   = 2 * time.Second
	blockedObservation = 50 * time.Millisecond
)

// SentFrame is one frame observed after the adapter accepted it for transport.
// Terminal remains explicit because terminal delivery has stronger completion
// and lifecycle semantics than an ordinary frame.
type SentFrame struct {
	Frame    framechannel.Frame
	Terminal bool
}

// Fixture projects the peer side of one freshly opened adapter. The callbacks
// let the suite drive remote events while keeping transport construction,
// framing, and saturation mechanics inside the implementing package.
type Fixture struct {
	Channel framechannel.Channel

	ReceiveSent     func(context.Context) (SentFrame, error)
	Deliver         func(framechannel.Frame) error
	DeliverTerminal func(framechannel.Frame) error
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
	require(t, factory != nil, "channeltest: factory must not be nil")

	t.Run("state-and-frame-bounds", func(t *testing.T) {
		fixture := openFixture(t, factory)
		state := fixture.Channel.State()
		require(t, state == framechannel.Open, "initial state = %v, want Open", state)

		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		err := fixture.Channel.Send(canceled, framechannel.Frame{1})
		require(t, errors.Is(err, context.Canceled), "Send with canceled context = %v, want context.Canceled", err)
		require(t, fixture.Channel.Send(context.Background(), nil) != nil, "Send accepted an empty frame")
		require(t, fixture.Channel.SendTerminal(context.Background(), nil) != nil, "SendTerminal accepted an empty frame")
		oversize := make(framechannel.Frame, framechannel.MaxFrameSize+1)
		require(t, fixture.Channel.Send(context.Background(), oversize) != nil, "Send accepted an oversized frame")
		require(t, fixture.Channel.SendTerminal(context.Background(), oversize) != nil, "SendTerminal accepted an oversized frame")
		state = fixture.Channel.State()
		require(t, state == framechannel.Open, "invalid input changed state to %v", state)

		want := patternedFrame(0x31, framechannel.MaxFrameSize)
		err = fixture.Channel.Send(context.Background(), want)
		require(t, err == nil, "Send maximum frame: %v", err)
		got := receiveSent(t, fixture)
		require(t, !got.Terminal && bytes.Equal(got.Frame, want),
			"maximum frame mismatch: terminal=%v bytes=%d", got.Terminal, len(got.Frame))
	})

	t.Run("payload-ownership", func(t *testing.T) {
		fixture := openFixture(t, factory)

		// Frame buffers remain caller-owned. Adapters must snapshot accepted
		// payloads so callers can immediately reuse pooled buffers without racing
		// transport writers or changing bytes already queued for delivery.
		outbound := patternedFrame(0x35, 257)
		wantOutbound := append(framechannel.Frame(nil), outbound...)
		err := fixture.Channel.Send(context.Background(), outbound)
		require(t, err == nil, "Send ownership frame: %v", err)
		mutate(outbound)
		gotOutbound := receiveSent(t, fixture)
		require(t, !gotOutbound.Terminal && bytes.Equal(gotOutbound.Frame, wantOutbound),
			"outbound frame changed after caller reused its buffer")

		inbound := patternedFrame(0x36, 257)
		wantInbound := append(framechannel.Frame(nil), inbound...)
		err = fixture.Deliver(inbound)
		require(t, err == nil, "deliver ownership frame: %v", err)
		mutate(inbound)
		require(t, bytes.Equal(receiveFrame(t, fixture.Channel), wantInbound),
			"inbound frame changed after peer reused its buffer")

		terminal := patternedFrame(0x37, 64)
		wantTerminal := append(framechannel.Frame(nil), terminal...)
		err = fixture.DeliverTerminal(terminal)
		require(t, err == nil, "deliver ownership terminal: %v", err)
		mutate(terminal)
		require(t, bytes.Equal(receiveFrame(t, fixture.Channel), wantTerminal),
			"inbound terminal changed after peer reused its buffer")
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
	})

	t.Run("backpressure-cancellation", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- fixture.Channel.Send(ctx, framechannel.Frame{0x42})
		}()
		select {
		case err := <-result:
			t.Fatalf("saturated Send returned before cancellation: %v", err)
		case <-time.After(blockedObservation):
		}
		cancel()
		select {
		case err := <-result:
			require(t, errors.Is(err, context.Canceled),
				"canceled saturated Send = %v, want context.Canceled", err)
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
			require(t, err == nil, "Send after capacity recovery: %v", err)
		case <-time.After(operationTimeout):
			t.Fatal("Send did not resume after capacity recovery")
		}
		receiveSentFrame(t, fixture, want)
	})

	t.Run("backpressure-remote-close", func(t *testing.T) {
		fixture := openFixture(t, factory)
		fixture.SaturateSends(t)
		result := make(chan error, 1)
		go func() { result <- fixture.Channel.Send(context.Background(), framechannel.Frame{0x48}) }()
		select {
		case err := <-result:
			t.Fatalf("saturated Send returned before remote close: %v", err)
		case <-time.After(blockedObservation):
		}
		err := fixture.RemoteClose()
		require(t, err == nil, "remote close while Send blocked: %v", err)
		select {
		case err := <-result:
			require(t, err != nil, "blocked Send succeeded after remote close")
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
		require(t, got.Terminal && bytes.Equal(got.Frame, want),
			"terminal mismatch: terminal=%v bytes=%x", got.Terminal, got.Frame)
		select {
		case err := <-result:
			require(t, err == nil, "SendTerminal: %v", err)
		case <-time.After(operationTimeout):
			t.Fatal("SendTerminal did not complete after peer delivery")
		}
		assertClosed(t, fixture.Channel)
		assertRecvClosed(t, fixture.Channel)
		require(t, fixture.Channel.Send(context.Background(), framechannel.Frame{1}) != nil,
			"Send succeeded after terminal")
		require(t, fixture.Channel.SendTerminal(context.Background(), framechannel.Frame{1}) != nil,
			"second terminal succeeded")
		err := fixture.Channel.Close()
		require(t, err == nil, "Close after terminal: %v", err)
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
			require(t, err == nil, "SendTerminal after release: %v", err)
		case <-time.After(operationTimeout):
			t.Fatal("SendTerminal did not complete after release")
		}
		select {
		case err := <-closeResult:
			require(t, err == nil, "Close after terminal: %v", err)
		case <-time.After(operationTimeout):
			t.Fatal("Close remained blocked after terminal completion")
		}
		assertClosed(t, fixture.Channel)
	})

	t.Run("inbound-terminal-before-close", func(t *testing.T) {
		fixture := openFixture(t, factory)
		ordinary := patternedFrame(0x61, framechannel.MaxFrameSize)
		terminal := patternedFrame(0x62, 64)
		err := fixture.Deliver(ordinary)
		require(t, err == nil, "deliver ordinary frame: %v", err)
		err = fixture.DeliverTerminal(terminal)
		require(t, err == nil, "deliver terminal frame: %v", err)
		// Late peer traffic may be reported or silently discarded, but it must
		// never revive the stream or appear after the terminal.
		_ = fixture.Deliver(framechannel.Frame{0xff})

		require(t, bytes.Equal(receiveFrame(t, fixture.Channel), ordinary),
			"ordinary frame changed before delivery")
		require(t, bytes.Equal(receiveFrame(t, fixture.Channel), terminal),
			"terminal was not the final received frame")
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
	})

	t.Run("close-idempotence", func(t *testing.T) {
		fixture := openFixture(t, factory)
		err := fixture.Channel.Close()
		require(t, err == nil, "first Close: %v", err)
		err = fixture.Channel.Close()
		require(t, err == nil, "second Close: %v", err)
		assertClosed(t, fixture.Channel)
		assertRecvClosed(t, fixture.Channel)
		require(t, fixture.Channel.Send(context.Background(), framechannel.Frame{1}) != nil,
			"Send succeeded after Close")
	})

	t.Run("remote-close-and-late-traffic", func(t *testing.T) {
		fixture := openFixture(t, factory)
		err := fixture.RemoteClose()
		require(t, err == nil, "remote close: %v", err)
		_ = fixture.Deliver(framechannel.Frame{1})
		assertRecvClosed(t, fixture.Channel)
		assertClosed(t, fixture.Channel)
		require(t, fixture.Channel.Send(context.Background(), framechannel.Frame{1}) != nil,
			"Send succeeded after remote close")
	})
}

func openFixture(t *testing.T, factory Factory) Fixture {
	t.Helper()
	fixture := factory(t)
	fixtureErr := validateFixture(fixture)
	require(t, fixtureErr == nil, "%v", fixtureErr)
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
	require(t, err == nil, "receive sent frame: %v", err)
	return got
}

func receiveSentFrame(t *testing.T, fixture Fixture, want framechannel.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for {
		got, err := fixture.ReceiveSent(ctx)
		require(t, err == nil, "receive recovered frame: %v", err)
		if !got.Terminal && bytes.Equal(got.Frame, want) {
			return
		}
	}
}

func receiveSentTerminal(t *testing.T, fixture Fixture, want framechannel.Frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	for {
		got, err := fixture.ReceiveSent(ctx)
		require(t, err == nil, "receive terminal frame: %v", err)
		if got.Terminal && bytes.Equal(got.Frame, want) {
			return
		}
	}
}

func receiveFrame(t *testing.T, channel framechannel.Channel) framechannel.Frame {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		require(t, ok, "Recv closed before expected frame")
		return frame
	case <-time.After(operationTimeout):
		t.Fatal("timeout waiting for received frame")
		return nil
	}
}

func assertRecvClosed(t *testing.T, channel framechannel.Channel) {
	t.Helper()
	select {
	case frame, ok := <-channel.Recv():
		require(t, !ok, "Recv yielded frame after terminal close: %x", frame)
	case <-time.After(operationTimeout):
		t.Fatal("Recv did not close")
	}
}

func assertClosed(t *testing.T, channel framechannel.Channel) {
	t.Helper()
	require(t, channel.State() == framechannel.Closed, "state = %v, want Closed", channel.State())
}

func validateFixture(fixture Fixture) error {
	requirements := []struct {
		name    string
		present bool
	}{
		{name: "Channel", present: fixture.Channel != nil},
		{name: "ReceiveSent", present: fixture.ReceiveSent != nil},
		{name: "Deliver", present: fixture.Deliver != nil},
		{name: "DeliverTerminal", present: fixture.DeliverTerminal != nil},
		{name: "RemoteClose", present: fixture.RemoteClose != nil},
		{name: "SaturateSends", present: fixture.SaturateSends != nil},
		{name: "ReleaseSends", present: fixture.ReleaseSends != nil},
		{name: "Cleanup", present: fixture.Cleanup != nil},
	}
	for _, requirement := range requirements {
		if !requirement.present {
			return fmt.Errorf("channeltest: fixture %s must not be nil", requirement.name)
		}
	}
	return nil
}

func require(t testing.TB, condition bool, format string, args ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(format, args...)
	}
}

func patternedFrame(marker byte, size int) framechannel.Frame {
	frame := make(framechannel.Frame, size)
	if size == 0 {
		return frame
	}
	frame[0] = marker
	for i := 1; i < len(frame); i++ {
		frame[i] = byte((i*31 + 17) % 251)
	}
	return frame
}

func mutate(frame framechannel.Frame) {
	for i := range frame {
		frame[i] ^= 0xff
	}
}
