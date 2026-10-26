package sessioncore

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/framechannel"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

func newMemoryChannel(t *testing.T) *memoryChannel {
	t.Helper()
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = channel.Close()
		_ = peer.Close()
	})
	return channel
}

type memoryPipe struct {
	mu     sync.Mutex
	inbox  [2]chan framechannel.Frame
	closed [2]bool
}

type deterministicReader struct {
	mu   sync.Mutex
	next byte
}

func (reader *deterministicReader) Read(destination []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	for index := range destination {
		destination[index] = reader.next
	}
	reader.next++
	if reader.next == 0 {
		reader.next = 1
	}
	return len(destination), nil
}

type memoryChannel struct {
	pipe      *memoryPipe
	index     int
	recvCalls atomic.Int32
	onSend    func(framechannel.Frame)
}

func newMemoryChannelPair() (*memoryChannel, *memoryChannel) {
	pipe := &memoryPipe{inbox: [2]chan framechannel.Frame{make(chan framechannel.Frame, 2_048), make(chan framechannel.Frame, 2_048)}}
	return &memoryChannel{pipe: pipe, index: 0}, &memoryChannel{pipe: pipe, index: 1}
}

func (channel *memoryChannel) Send(_ context.Context, frame framechannel.Frame) error {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	target := 1 - channel.index
	if channel.pipe.closed[channel.index] || channel.pipe.closed[target] {
		return io.ErrClosedPipe
	}
	channel.pipe.inbox[target] <- bytes.Clone(frame)
	if channel.onSend != nil {
		channel.onSend(bytes.Clone(frame))
	}
	return nil
}

func (channel *memoryChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

func (channel *memoryChannel) Recv() <-chan framechannel.Frame {
	channel.recvCalls.Add(1)
	return channel.pipe.inbox[channel.index]
}

func (channel *memoryChannel) State() framechannel.ChannelState {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	if channel.pipe.closed[channel.index] {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (channel *memoryChannel) Close() error {
	channel.pipe.mu.Lock()
	if !channel.pipe.closed[channel.index] {
		channel.pipe.closed[channel.index] = true
		close(channel.pipe.inbox[channel.index])
	}
	channel.pipe.mu.Unlock()
	return nil
}

func id16[T ~[16]byte](value byte) T {
	var result T
	for index := range result {
		result[index] = value
	}
	return result
}
