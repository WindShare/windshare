package webrtc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/internal/testnetwork"
)

const (
	benchmarkPeerTimeout     = 10 * time.Second
	benchmarkTransferTimeout = 2 * time.Minute
	benchmarkKiB             = 1024
	benchmarkMiB             = 1024 * benchmarkKiB
)

var benchmarkChunkSizes = [...]int{
	benchmarkKiB,
	64 * benchmarkKiB,
	benchmarkMiB,
	4 * benchmarkMiB,
}

// BenchmarkPionChunkTransfer measures the production adapter rather than raw
// Pion. Each chunk is split at WindShare's frame boundary so the 1 KiB..4 MiB
// geometry exercises the same bufferedAmount hysteresis used by real sessions.
func BenchmarkPionChunkTransfer(b *testing.B) {
	testnetwork.RequireOSNetwork(b)
	for _, chunkBytes := range benchmarkChunkSizes {
		b.Run(fmt.Sprintf("chunk_%dKiB", chunkBytes/benchmarkKiB), func(b *testing.B) {
			benchmarkPionChunkTransfer(b, chunkBytes)
		})
	}
}

func benchmarkPionChunkTransfer(b *testing.B, chunkBytes int) {
	left, right, raw := newBenchmarkPionPair(b)

	frames, err := benchmarkChunkFrames(chunkBytes)
	if err != nil {
		b.Fatalf("construct benchmark chunk frames: %v", err)
	}
	wireBytes := benchmarkFrameBytes(frames)
	totalFrames := b.N * len(frames)
	received := make(chan benchmarkReceiveResult, 1)
	go receiveBenchmarkFrames(right, totalFrames, received)

	ctx, cancel := context.WithTimeout(context.Background(), benchmarkTransferTimeout)
	defer cancel()
	var peakBuffered uint64

	b.ReportAllocs()
	b.SetBytes(int64(chunkBytes))
	b.ResetTimer()
	for range b.N {
		for _, frame := range frames {
			if err := left.Send(ctx, frame); err != nil {
				b.Fatalf("send benchmark frame: %v", err)
			}
			peakBuffered = max(peakBuffered, raw.BufferedAmount())
		}
	}
	var result benchmarkReceiveResult
	select {
	case result = <-received:
	case <-ctx.Done():
		b.Fatalf("receive benchmark frames: %v", ctx.Err())
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
	b.ReportMetric(float64(wireBytes), "wire-B/chunk")
	b.ReportMetric(float64(peakBuffered), "peak-buffered-B")
	b.ReportMetric(float64(defaultLowWaterBytes), "low-water-B")
	b.ReportMetric(float64(defaultHighWaterBytes), "high-water-B")
}

type benchmarkReceiveResult struct {
	bytes int64
	err   error
}

func receiveBenchmarkFrames(channel *Channel, frames int, result chan<- benchmarkReceiveResult) {
	var received int64
	for range frames {
		frame, ok := <-channel.Recv()
		if !ok {
			result <- benchmarkReceiveResult{err: fmt.Errorf(
				"benchmark receive closed after %d bytes: %w",
				received,
				channel.Err(),
			)}
			return
		}
		received += int64(len(frame))
	}
	result <- benchmarkReceiveResult{bytes: received}
}

func benchmarkChunkFrames(chunkBytes int) ([]session.Frame, error) {
	block := make([]byte, chunkBytes)
	for index := range block {
		block[index] = byte((index*31 + chunkBytes) % 251)
	}
	return session.SplitBlockCT(0, block, session.MaxBlockPayload)
}

func benchmarkFrameBytes(frames []session.Frame) int {
	total := 0
	for _, frame := range frames {
		total += len(frame)
	}
	return total
}

type benchmarkRemoteChannel struct {
	channel *Channel
	err     error
}

func newBenchmarkPionPair(b *testing.B) (
	*Channel,
	*Channel,
	*pion.DataChannel,
) {
	b.Helper()
	api := benchmarkLoopbackAPI()
	leftPeer := benchmarkPeerConnection(b, api)
	rightPeer := benchmarkPeerConnection(b, api)

	remoteReady := make(chan benchmarkRemoteChannel, 1)
	rightPeer.OnDataChannel(func(raw *pion.DataChannel) {
		channel, err := NewChannel(raw)
		remoteReady <- benchmarkRemoteChannel{channel: channel, err: err}
	})

	leftRaw, err := leftPeer.CreateDataChannel(ChannelLabel, DefaultDataChannelInit())
	if err != nil {
		b.Fatalf("create benchmark DataChannel: %v", err)
	}
	left, err := NewChannel(leftRaw)
	if err != nil {
		b.Fatalf("wrap benchmark DataChannel: %v", err)
	}
	benchmarkNegotiatePeers(b, leftPeer, rightPeer)

	var remote benchmarkRemoteChannel
	select {
	case remote = <-remoteReady:
	case <-time.After(benchmarkPeerTimeout):
		b.Fatal("timeout waiting for benchmark remote DataChannel")
	}
	if remote.err != nil {
		b.Fatalf("wrap benchmark remote DataChannel: %v", remote.err)
	}
	benchmarkWaitOpened(b, left)
	benchmarkWaitOpened(b, remote.channel)
	return left, remote.channel, leftRaw
}

func benchmarkLoopbackAPI() *pion.API {
	var setting pion.SettingEngine
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetIPFilter(func(ip net.IP) bool {
		return ip.IsLoopback()
	})
	return pion.NewAPI(pion.WithSettingEngine(setting))
}

func benchmarkPeerConnection(b *testing.B, api *pion.API) *pion.PeerConnection {
	b.Helper()
	testnetwork.RequireOSNetwork(b)
	peer, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		b.Fatalf("create benchmark PeerConnection: %v", err)
	}
	b.Cleanup(func() {
		_ = peer.Close()
	})
	return peer
}

func benchmarkNegotiatePeers(
	b *testing.B,
	offerer *pion.PeerConnection,
	answerer *pion.PeerConnection,
) {
	b.Helper()
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		b.Fatalf("create benchmark offer: %v", err)
	}
	offerGathered := pion.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		b.Fatalf("set benchmark local offer: %v", err)
	}
	benchmarkWaitGathering(b, offerGathered)
	benchmarkRequireLoopbackCandidates(b, "offer", offerer.LocalDescription())
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		b.Fatalf("set benchmark remote offer: %v", err)
	}

	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		b.Fatalf("create benchmark answer: %v", err)
	}
	answerGathered := pion.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(answer); err != nil {
		b.Fatalf("set benchmark local answer: %v", err)
	}
	benchmarkWaitGathering(b, answerGathered)
	benchmarkRequireLoopbackCandidates(b, "answer", answerer.LocalDescription())
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		b.Fatalf("set benchmark remote answer: %v", err)
	}
}

func benchmarkRequireLoopbackCandidates(
	b *testing.B,
	label string,
	description *pion.SessionDescription,
) {
	b.Helper()
	if description == nil {
		b.Fatalf("benchmark %s description is unavailable", label)
		return
	}
	found := false
	for line := range strings.SplitSeq(description.SDP, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			b.Fatalf("benchmark %s candidate is malformed: %q", label, line)
			return
		}
		address := net.ParseIP(fields[4])
		if address == nil || !address.IsLoopback() {
			b.Fatalf(
				"benchmark %s candidate address %q is not loopback",
				label,
				fields[4],
			)
			return
		}
		found = true
	}
	if !found {
		b.Fatalf("benchmark %s has no loopback ICE candidate", label)
	}
}

func benchmarkWaitGathering(b *testing.B, done <-chan struct{}) {
	b.Helper()
	select {
	case <-done:
	case <-time.After(benchmarkPeerTimeout):
		b.Fatal("timeout waiting for benchmark ICE gathering")
	}
}

func benchmarkWaitOpened(b *testing.B, channel *Channel) {
	b.Helper()
	select {
	case <-channel.Opened():
	case <-channel.Done():
		b.Fatalf("benchmark channel closed before Open: %v", channel.Err())
	case <-time.After(benchmarkPeerTimeout):
		b.Fatal("timeout waiting for benchmark channel Open")
	}
}
