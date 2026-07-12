package connectivity

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/session"
)

const (
	performanceWindowKiB = 1024
	performanceWindowMiB = 1024 * performanceWindowKiB
)

var performanceWindowChunkSizes = [...]int{
	performanceWindowKiB,
	64 * performanceWindowKiB,
	performanceWindowMiB,
	4 * performanceWindowMiB,
}

// BenchmarkSenderRequestWindowZeroPressure isolates request-window orchestration,
// frame splitting, and copying. Its channels admit every frame immediately, so the
// result deliberately says nothing about transport backpressure or path isolation.
func BenchmarkSenderRequestWindowZeroPressure(b *testing.B) {
	indices := make([]uint64, session.InFlightWindow)
	for index := range indices {
		indices[index] = uint64(index)
	}
	request, err := session.EncodeRequest(indices)
	if err != nil {
		b.Fatalf("encode request window: %v", err)
	}

	for _, chunkBytes := range performanceWindowChunkSizes {
		b.Run(fmt.Sprintf("chunk_%dKiB", chunkBytes/performanceWindowKiB), func(b *testing.B) {
			block := make([]byte, chunkBytes)
			for index := range block {
				block[index] = byte((index*13 + chunkBytes) % 251)
			}
			store := performanceBlockStore{block: block}
			sealer := performanceIdentitySealer{}
			measurement := newZeroPressureMeasurement(chunkBytes)

			b.ReportAllocs()
			b.SetBytes(2 * int64(session.InFlightWindow) * int64(chunkBytes))
			b.ResetTimer()
			for range b.N {
				relayChannel := newZeroPressureSendChannel(request)
				peerChannel := newZeroPressureSendChannel(request)
				sender, newErr := NewSender(
					performanceAnswerFactory{channel: peerChannel},
					SenderOptions{},
				)
				if newErr != nil {
					b.Fatalf("construct performance sender: %v", newErr)
				}
				if serveErr := sender.ServeReceiver(
					context.Background(),
					relayChannel,
					performanceSignaling{},
					store,
					sealer,
				); serveErr != nil {
					b.Fatalf("serve performance receiver: %v", serveErr)
				}
				wantBytes := int64(session.InFlightWindow * measurement.wireBytesPerChunk)
				if relayChannel.sentBytes.Load() != wantBytes {
					b.Fatalf(
						"relay sent bytes = %d, want %d",
						relayChannel.sentBytes.Load(),
						wantBytes,
					)
				}
				if peerChannel.sentBytes.Load() != wantBytes {
					b.Fatalf(
						"peer sent bytes = %d, want %d",
						peerChannel.sentBytes.Load(),
						wantBytes,
					)
				}
			}
			b.ReportMetric(float64(measurement.requestWindowBlocks), "request-window-blocks")
			b.ReportMetric(float64(measurement.framesPerChunk), "frames/chunk")
			b.ReportMetric(float64(measurement.wireBytesPerChunk), "wire-B/chunk")
			b.ReportMetric(float64(measurement.paths), "paths")
			b.ReportMetric(float64(measurement.backpressuredPaths), "backpressured-paths")
		})
	}
}

type zeroPressureMeasurement struct {
	requestWindowBlocks int
	framesPerChunk      int
	wireBytesPerChunk   int
	paths               int
	backpressuredPaths  int
}

func newZeroPressureMeasurement(chunkBytes int) zeroPressureMeasurement {
	framesPerChunk := (chunkBytes + session.MaxBlockPayload - 1) / session.MaxBlockPayload
	frameHeaderBytes := session.MaxFrameSize - session.MaxBlockPayload
	return zeroPressureMeasurement{
		requestWindowBlocks: session.InFlightWindow,
		framesPerChunk:      framesPerChunk,
		wireBytesPerChunk:   chunkBytes + framesPerChunk*frameHeaderBytes,
		paths:               2,
		backpressuredPaths:  0,
	}
}

func TestZeroPressureMeasurementGeometry(t *testing.T) {
	t.Parallel()
	wantFrames := [...]int{1, 2, 17, 65}
	wantWireBytes := [...]int{1_042, 65_572, 1_048_882, 4_195_474}
	for index, chunkBytes := range performanceWindowChunkSizes {
		measurement := newZeroPressureMeasurement(chunkBytes)
		if measurement.requestWindowBlocks != session.InFlightWindow ||
			measurement.framesPerChunk != wantFrames[index] ||
			measurement.wireBytesPerChunk != wantWireBytes[index] ||
			measurement.paths != 2 || measurement.backpressuredPaths != 0 {
			t.Fatalf("chunk %d measurement = %+v", chunkBytes, measurement)
		}
	}
}

type performanceBlockStore struct {
	block []byte
}

func (s performanceBlockStore) ReadBlock(index uint64) ([]byte, error) {
	if index >= session.InFlightWindow {
		return nil, fmt.Errorf("block index %d is outside benchmark window", index)
	}
	return s.block, nil
}

func (performanceBlockStore) BlockCount() uint64 {
	return session.InFlightWindow
}

type performanceIdentitySealer struct{}

func (performanceIdentitySealer) Seal(_ uint64, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

type performanceAnswerFactory struct {
	channel PeerChannel
}

func (f performanceAnswerFactory) Answer(context.Context, Signaling) (PeerChannel, error) {
	return f.channel, nil
}

type performanceSignaling struct{}

func (performanceSignaling) Send(ctx context.Context, _ Signal) error {
	return ctx.Err()
}

func (performanceSignaling) Receive(ctx context.Context) (Signal, error) {
	<-ctx.Done()
	return Signal{}, ctx.Err()
}

type zeroPressureSendChannel struct {
	recv      chan session.Frame
	opened    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	state     atomic.Uint32
	sentBytes atomic.Int64
}

func newZeroPressureSendChannel(request session.Frame) *zeroPressureSendChannel {
	recv := make(chan session.Frame, 1)
	recv <- append(session.Frame(nil), request...)
	close(recv)
	opened := make(chan struct{})
	close(opened)
	channel := &zeroPressureSendChannel{
		recv:   recv,
		opened: opened,
		done:   make(chan struct{}),
	}
	channel.state.Store(uint32(session.Open))
	return channel
}

func (c *zeroPressureSendChannel) Opened() <-chan struct{} { return c.opened }

func (c *zeroPressureSendChannel) Done() <-chan struct{} { return c.done }

func (c *zeroPressureSendChannel) Recv() <-chan session.Frame { return c.recv }

func (c *zeroPressureSendChannel) State() session.ChannelState {
	return session.ChannelState(c.state.Load())
}

func (*zeroPressureSendChannel) Err() error { return nil }

func (c *zeroPressureSendChannel) Send(ctx context.Context, frame session.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(frame) == 0 || len(frame) > session.MaxFrameSize {
		return fmt.Errorf("performance channel frame has %d bytes", len(frame))
	}
	if c.State() != session.Open {
		return session.ErrSessionClosed
	}
	c.sentBytes.Add(int64(len(frame)))
	return nil
}

func (c *zeroPressureSendChannel) SendTerminal(
	ctx context.Context,
	frame session.Frame,
) error {
	if err := c.Send(ctx, frame); err != nil {
		return err
	}
	return c.Close()
}

func (c *zeroPressureSendChannel) Close() error {
	c.closeOnce.Do(func() {
		c.state.Store(uint32(session.Closed))
		close(c.done)
	})
	return nil
}
