package relay

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

const (
	performanceKiB             = 1024
	performanceMiB             = 1024 * performanceKiB
	performanceTransferTimeout = 2 * time.Minute
	performanceDialTimeout     = 30 * time.Second
	performanceShareID         = "ZDVfYmVuY2htYXJr"
)

var performanceChunkSizes = [...]int{
	performanceKiB,
	64 * performanceKiB,
	performanceMiB,
	4 * performanceMiB,
}

// BenchmarkRelayChunkTransfer includes both client WebSockets and the in-process
// relay. Setup is outside the timer; each operation is one protocol-valid chunk
// represented by one or more unchanged WindShare frames.
func BenchmarkRelayChunkTransfer(b *testing.B) {
	testnetwork.RequireOSNetwork(b)
	for _, chunkBytes := range performanceChunkSizes {
		b.Run(fmt.Sprintf("chunk_%dKiB", chunkBytes/performanceKiB), func(b *testing.B) {
			benchmarkRelayChunkTransfer(b, chunkBytes)
		})
	}
}

func benchmarkRelayChunkTransfer(b *testing.B, chunkBytes int) {
	senderChannel, receiverChannel := newPerformanceRelayPair(b)

	frames, err := performanceChunkFrames(chunkBytes)
	if err != nil {
		b.Fatalf("construct relay benchmark frames: %v", err)
	}
	wireBytes := performanceFrameBytes(frames)
	relayWireBytes := wireBytes + len(frames)*protocol.ForwardOverheadBytes
	delivered := make(chan struct{}, session.InFlightWindow)
	received := make(chan relayReceiveResult, 1)
	go receiveRelayBenchmarkChunks(receiverChannel, b.N, len(frames), delivered, received)

	ctx, cancel := context.WithTimeout(context.Background(), performanceTransferTimeout)
	defer cancel()
	b.ReportAllocs()
	b.SetBytes(int64(chunkBytes))
	b.ResetTimer()
	inFlightChunks := 0
	for range b.N {
		for _, frame := range frames {
			if err := senderChannel.Send(ctx, frame); err != nil {
				b.Fatalf("send relay benchmark frame: %v", err)
			}
		}
		inFlightChunks++
		if inFlightChunks == session.InFlightWindow {
			waitRelayBenchmarkDelivery(b, ctx, delivered)
			inFlightChunks--
		}
	}
	for inFlightChunks > 0 {
		waitRelayBenchmarkDelivery(b, ctx, delivered)
		inFlightChunks--
	}
	var result relayReceiveResult
	select {
	case result = <-received:
	case <-ctx.Done():
		b.Fatalf("receive relay benchmark frames: %v", ctx.Err())
	}
	b.StopTimer()
	if result.err != nil {
		b.Fatal(result.err)
	}
	wantBytes := int64(b.N) * int64(wireBytes)
	if result.bytes != wantBytes {
		b.Fatalf("received bytes = %d, want %d", result.bytes, wantBytes)
	}
	b.ReportMetric(float64(len(frames)), "frames/chunk")
	b.ReportMetric(float64(wireBytes), "block-wire-B/chunk")
	b.ReportMetric(float64(relayWireBytes), "relay-wire-B/hop")
	b.ReportMetric(float64(2*relayWireBytes), "relay-wire-B/transfer")
	b.ReportMetric(float64(dataLaneFrames), "client-queue-frames")
	b.ReportMetric(float64(forward.DefaultSessionQueueFrames), "server-queue-frames")
	b.ReportMetric(float64(session.InFlightWindow), "in-flight-chunks")
	b.ReportMetric(
		float64(dataLaneFrames/len(frames)),
		"client-complete-chunks",
	)
	b.ReportMetric(
		float64(forward.DefaultSessionQueueFrames/len(frames)),
		"server-complete-chunks",
	)
}

type relayReceiveResult struct {
	bytes int64
	err   error
}

func receiveRelayBenchmarkChunks(
	channel *Channel,
	chunks int,
	framesPerChunk int,
	delivered chan<- struct{},
	result chan<- relayReceiveResult,
) {
	var received int64
	for range chunks {
		for range framesPerChunk {
			frame, ok := <-channel.Recv()
			if !ok {
				result <- relayReceiveResult{err: fmt.Errorf(
					"relay benchmark receive closed after %d bytes: %w",
					received,
					channel.Err(),
				)}
				return
			}
			received += int64(len(frame))
		}
		delivered <- struct{}{}
	}
	result <- relayReceiveResult{bytes: received}
}

func waitRelayBenchmarkDelivery(
	b *testing.B,
	ctx context.Context,
	delivered <-chan struct{},
) {
	b.Helper()
	select {
	case <-delivered:
	case <-ctx.Done():
		b.Fatalf("wait for relay benchmark delivery: %v", ctx.Err())
	}
}

func performanceChunkFrames(chunkBytes int) ([]session.Frame, error) {
	block := make([]byte, chunkBytes)
	for index := range block {
		block[index] = byte((index*17 + chunkBytes) % 251)
	}
	return session.SplitBlockCT(0, block, session.MaxBlockPayload)
}

func performanceFrameBytes(frames []session.Frame) int {
	total := 0
	for _, frame := range frames {
		total += len(frame)
	}
	return total
}

func newPerformanceRelayPair(b *testing.B) (*Channel, *Channel) {
	b.Helper()
	testnetwork.RequireOSNetwork(b)
	hub := signaling.NewHub(signaling.Config{})
	server := httptest.NewServer(httpapi.NewHandler(httpapi.Config{Hub: hub}))
	ctx, cancel := context.WithTimeout(context.Background(), performanceDialTimeout)
	var sender *SenderConn
	var receiver *ReceiverConn
	b.Cleanup(func() {
		if receiver != nil {
			_ = receiver.Close()
		}
		if sender != nil {
			_ = sender.Close()
		}
		cancel()
		server.Close()
		hub.Close()
	})

	var err error
	sender, err = DialSender(ctx, SenderConfig{
		RelayURL:          server.URL,
		ShareID:           performanceShareID,
		SealedManifest:    bytes.Repeat([]byte{0x5a}, 256),
		ResumeToken:       bytes.Repeat([]byte{0x2c}, protocol.ResumeTokenBytes),
		KeepaliveInterval: time.Hour,
		ReconnectGrace:    performanceDialTimeout,
	})
	if err != nil {
		b.Fatalf("dial performance sender: %v", err)
	}
	receiver, err = DialReceiver(ctx, ReceiverConfig{
		RelayURL:          server.URL,
		ShareID:           performanceShareID,
		KeepaliveInterval: time.Hour,
		JoinRetryWindow:   performanceDialTimeout,
	})
	if err != nil {
		b.Fatalf("dial performance receiver: %v", err)
	}

	request, err := session.EncodeRequest([]uint64{0})
	if err != nil {
		b.Fatalf("encode performance session primer: %v", err)
	}
	if err := receiver.Channel().Send(ctx, request); err != nil {
		b.Fatalf("send performance session primer: %v", err)
	}

	var senderChannel *Channel
	select {
	case senderChannel = <-sender.Sessions():
	case <-ctx.Done():
		b.Fatalf("wait for performance sender session: %v", ctx.Err())
	}
	select {
	case <-senderChannel.Recv():
	case <-ctx.Done():
		b.Fatalf("drain performance session primer: %v", ctx.Err())
	}
	return senderChannel, receiver.Channel()
}

// TestSharedForwardQueueChunkPolicy turns the queue formula into executable
// evidence without coupling it to core/chunk. Queue capacity remains transport
// policy: production BLOCK framing makes larger chunks consume more frame slots.
func TestSharedForwardQueueChunkPolicy(t *testing.T) {
	for _, chunkBytes := range performanceChunkSizes {
		t.Run(fmt.Sprintf("chunk_%dKiB", chunkBytes/performanceKiB), func(t *testing.T) {
			frames, err := performanceChunkFrames(chunkBytes)
			if err != nil {
				t.Fatalf("construct queue-policy frames: %v", err)
			}
			wireBytes := performanceFrameBytes(frames)
			frameHeaderBytes := session.MaxFrameSize - session.MaxBlockPayload
			wantWireBytes := chunkBytes + len(frames)*frameHeaderBytes
			if wireBytes != wantWireBytes {
				t.Fatalf("wire bytes = %d, want %d", wireBytes, wantWireBytes)
			}
			for index, frame := range frames {
				if len(frame) > session.MaxFrameSize {
					t.Fatalf(
						"frame %d has %d bytes, maximum is %d",
						index,
						len(frame),
						session.MaxFrameSize,
					)
				}
			}

			writer := newPerformanceGateWriter()
			pump := forward.NewPump(writer, forward.Options{})
			t.Cleanup(func() {
				writer.releaseWrite()
				pump.Close()
				<-pump.Done()
			})
			sid := protocol.SessionID{byte(chunkBytes / performanceKiB), 0xd5}
			if got := pump.OpenSession(sid); got != forward.Enqueued {
				t.Fatalf("OpenSession = %v, want Enqueued", got)
			}

			if got := pump.EnqueueForward(sid, frames[0]); got != forward.Enqueued {
				t.Fatalf("prime writer = %v, want Enqueued", got)
			}
			writer.waitEntered(t)
			for index := range forward.DefaultSessionQueueFrames {
				frame := frames[index%len(frames)]
				if got := pump.EnqueueForward(sid, frame); got != forward.Enqueued {
					t.Fatalf("queue frame %d = %v, want Enqueued", index, got)
				}
			}
			if got := pump.EnqueueForward(sid, frames[0]); got != forward.Overflow {
				t.Fatalf("overflow result = %v, want Overflow", got)
			}

			completeChunks := forward.DefaultSessionQueueFrames / len(frames)
			t.Logf(
				"chunk=%d frame=%d frames/chunk=%d queue=%d complete_chunks=%d",
				chunkBytes,
				len(frames[0]),
				len(frames),
				forward.DefaultSessionQueueFrames,
				completeChunks,
			)
		})
	}
}

type performanceGateWriter struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newPerformanceGateWriter() *performanceGateWriter {
	return &performanceGateWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (*performanceGateWriter) WriteText([]byte) error { return nil }

func (w *performanceGateWriter) WriteBinary([]byte) error {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	return nil
}

func (w *performanceGateWriter) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-w.entered:
	case <-time.After(performanceDialTimeout):
		t.Fatal("timeout waiting for shared forward writer")
	}
}

func (w *performanceGateWriter) releaseWrite() {
	w.releaseOnce.Do(func() { close(w.release) })
}
