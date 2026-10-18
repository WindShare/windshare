package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	slowBenchmarkSend     = time.Millisecond
	benchmarkOrdinalBytes = 8
)

// An opt-in real-socket benchmark keeps timing assertions out of correctness gates.
// Both endpoints deliberately use production defaults; a smaller declaration would
// hide feedback policies that assume the relay grants the entire receive capacity.
func BenchmarkNativeRelayThroughput(b *testing.B) {
	binaryPath := os.Getenv("WINDSHARE_RELAY_BENCH_BINARY")
	if binaryPath == "" {
		b.Skip("set WINDSHARE_RELAY_BENCH_BINARY to a release relay-native binary")
	}
	for _, size := range []int{16 << 10, v2.MaxOpaqueCiphertextBytes} {
		for _, reverse := range []bool{false, true} {
			direction := "sender_to_receiver"
			if reverse {
				direction = "receiver_to_sender"
			}
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, direction), func(b *testing.B) {
				benchmarkNativeDelivery(b, binaryPath, size, reverse)
			})
		}
	}
}

type throughputSocket struct {
	*websocket.Conn
	feedback *atomic.Uint64
}

func (s *throughputSocket) Write(ctx context.Context, kind websocket.MessageType, frame []byte) error {
	err := s.Conn.Write(ctx, kind, frame)
	if err == nil && len(frame) >= len(v2.DeliveryConsumedMagic) && string(frame[:4]) == v2.DeliveryConsumedMagic {
		s.feedback.Add(1)
	}
	return err
}

func benchmarkPair(b *testing.B, binaryPath string) (*runtimePair, *atomic.Uint64) {
	b.Helper()
	process := start(binaryPath, filepath.Join(b.TempDir(), "relay.sqlite"), "127.0.0.1:0")
	b.Cleanup(process.kill)
	ctx := b.Context()
	var feedback atomic.Uint64
	dial := func(ctx context.Context, target string, header http.Header) (relayv2.BinarySocket, error) {
		socket, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose
		if err != nil {
			return nil, err
		}
		return &throughputSocket{Conn: socket, feedback: &feedback}, nil
	}
	f := newDeliveryFixture()
	sender, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: strings.TrimSuffix(process.endpoint, v2.WebSocketPath), Init: f.init.Registration, SenderPrivateKey: f.key,
		Descriptor: f.descriptor, ResumeToken: f.token, Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = sender.Close() })
	receiver, err := relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{
		RelayBaseURL: strings.TrimSuffix(process.endpoint, v2.WebSocketPath), ShareID: f.init.Registration.ShareID,
		Dial: relayv2.DialOptions{SocketDialer: dial},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = receiver.Close() })
	bootstrap := []byte("throughput bootstrap")
	if err := receiver.Channel().Send(ctx, bootstrap); err != nil {
		b.Fatal(err)
	}
	channel, err := sender.Accept(ctx)
	if err != nil {
		b.Fatal(err)
	}
	takeRuntime(ctx, channel, bootstrap)
	if err := channel.ConfirmAdmission(ctx); err != nil {
		b.Fatal(err)
	}
	return &runtimePair{sender: channel, receiver: receiver}, &feedback
}

func benchmarkNativeDelivery(b *testing.B, binaryPath string, size int, reverse bool) {
	pair, feedback := benchmarkPair(b, binaryPath)
	source, destination := pair.sender, pair.receiver.Channel()
	if reverse {
		source, destination = destination, source
	}
	payload := make([]byte, size)
	for index := range payload {
		payload[index] = byte(index)
	}
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	received := make(chan error, 1)
	go func() {
		err := verifyBenchmarkDelivery(ctx, destination, payload[benchmarkOrdinalBytes:], b.N)
		if err != nil {
			cancel()
		}
		received <- err
	}()
	before := feedback.Load()
	var slowSends uint64
	var longestSend time.Duration
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for ordinal := range b.N {
		binary.BigEndian.PutUint64(payload, uint64(ordinal))
		started := time.Now()
		if err := source.Send(ctx, payload); err != nil {
			b.Fatal(err)
		}
		elapsed := time.Since(started)
		longestSend = max(longestSend, elapsed)
		if elapsed >= slowBenchmarkSend {
			slowSends++
		}
	}
	if err := <-received; err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	b.ReportMetric(float64(feedback.Load()-before)/float64(b.N), "feedback/frame")
	b.ReportMetric(float64(slowSends)/float64(b.N), "send-over-1ms/frame")
	b.ReportMetric(float64(longestSend)/float64(time.Millisecond), "max-send-ms")
}

func verifyBenchmarkDelivery(ctx context.Context, destination *relayv2.Channel, body []byte, count int) error {
	for ordinal := range count {
		select {
		case frame, open := <-destination.Recv():
			if !open || len(frame) != benchmarkOrdinalBytes+len(body) || binary.BigEndian.Uint64(frame) != uint64(ordinal) ||
				!bytes.Equal(frame[benchmarkOrdinalBytes:], body) {
				return fmt.Errorf("delivery changed, reordered or closed at frame %d", ordinal)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
