package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

type m1cShare struct {
	relayURL string
	link     link.Link
	cancel   context.CancelFunc
	conn     *relay.SenderConn
	done     chan struct{}
	mu       sync.Mutex
	err      error
}

func startM1CShare(t *testing.T, sourceRoot string) *m1cShare {
	t.Helper()
	relayURL := startInProcRelay(t)
	snapshot, err := osfs.Walk([]string{sourceRoot})
	if err != nil {
		t.Fatal(err)
	}
	metas := make([]share.FileMeta, len(snapshot.Entries))
	for index, entry := range snapshot.Entries {
		metas[index] = share.FileMeta(entry)
	}
	sharer, err := share.NewSharer(
		metas,
		osfs.NewSource(snapshot),
		share.Options{ChunkSize: e2eBlockSizeInt},
	)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sharer.SealedManifest()
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, protocol.ResumeTokenBytes)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	conn, err := relay.DialSender(ctx, relay.SenderConfig{
		RelayURL:       relayURL,
		ShareID:        sharer.Link().ShareID,
		SealedManifest: sealed,
		ResumeToken:    token,
	})
	if err != nil {
		cancel()
		t.Fatalf("dial M1c sender: %v", err)
	}
	fixture := &m1cShare{
		relayURL: relayURL,
		link:     sharer.Link(),
		cancel:   cancel,
		conn:     conn,
		done:     make(chan struct{}),
	}
	go func() {
		runErr := serveSender(ctx, conn, sharer)
		fixture.mu.Lock()
		fixture.err = runErr
		fixture.mu.Unlock()
		close(fixture.done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case <-fixture.done:
		case <-time.After(procIOTimeout):
			t.Errorf("M1c share sender did not settle during cleanup")
		}
	})
	return fixture
}

func (s *m1cShare) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	case <-time.After(procIOTimeout):
		t.Fatal("M1c share sender did not settle")
		return nil
	}
}

func (s *m1cShare) stop(t *testing.T) error {
	t.Helper()
	s.cancel()
	_ = s.conn.Close()
	return s.wait(t)
}

type pathProbe struct {
	channel        session.FrameChannel
	recv           chan session.Frame
	stop           chan struct{}
	stopOnce       sync.Once
	requestFrames  atomic.Int64
	blockFrames    atomic.Int64
	errorFrames    atomic.Int64
	forwardingDone chan struct{}
}

func newPathProbe(channel session.FrameChannel) *pathProbe {
	probe := &pathProbe{
		channel:        channel,
		recv:           make(chan session.Frame),
		stop:           make(chan struct{}),
		forwardingDone: make(chan struct{}),
	}
	go probe.forward()
	return probe
}

func (p *pathProbe) forward() {
	defer close(p.forwardingDone)
	defer close(p.recv)
	for frame := range p.channel.Recv() {
		copyFrame := append(session.Frame(nil), frame...)
		select {
		case p.recv <- copyFrame:
			if len(copyFrame) > 0 {
				switch copyFrame[0] {
				case session.FrameBlock:
					p.blockFrames.Add(1)
				case session.FrameError:
					p.errorFrames.Add(1)
				}
			}
		case <-p.stop:
			return
		}
	}
}

func (p *pathProbe) Send(ctx context.Context, frame session.Frame) error {
	if err := p.channel.Send(ctx, frame); err != nil {
		return err
	}
	if len(frame) > 0 && frame[0] == session.FrameRequest {
		p.requestFrames.Add(1)
	}
	return nil
}

func (p *pathProbe) SendTerminal(ctx context.Context, frame session.Frame) error {
	return p.channel.SendTerminal(ctx, frame)
}

func (p *pathProbe) Recv() <-chan session.Frame { return p.recv }

func (p *pathProbe) State() session.ChannelState { return p.channel.State() }

func (p *pathProbe) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	err := p.channel.Close()
	<-p.forwardingDone
	return err
}

type pathProbeTestChannel struct {
	recv      chan session.Frame
	sendErr   error
	closeOnce sync.Once
}

func newPathProbeTestChannel() *pathProbeTestChannel {
	return &pathProbeTestChannel{recv: make(chan session.Frame)}
}

func (c *pathProbeTestChannel) Send(context.Context, session.Frame) error {
	return c.sendErr
}

func (c *pathProbeTestChannel) SendTerminal(context.Context, session.Frame) error {
	return c.sendErr
}

func (c *pathProbeTestChannel) Recv() <-chan session.Frame { return c.recv }

func (c *pathProbeTestChannel) State() session.ChannelState { return session.Open }

func (c *pathProbeTestChannel) Close() error {
	c.closeOnce.Do(func() { close(c.recv) })
	return nil
}

type m1cPath string

const (
	m1cRelayPath m1cPath = "relay"
	m1cPeerPath  m1cPath = "p2p"
)

type m1cReceiver struct {
	conn    *relay.ReceiverConn
	peer    connectivity.PeerChannel
	sink    *osfs.Sink
	plan    *share.TransferPlan
	session *session.ReceiveSession
	probe   *pathProbe
	outTree string
}

func prepareM1CReceiver(
	t *testing.T,
	ctx context.Context,
	shared *m1cShare,
	path m1cPath,
	decorate func(session.Sink) session.Sink,
) *m1cReceiver {
	t.Helper()
	conn, err := relay.DialReceiver(ctx, relay.ReceiverConfig{
		RelayURL: shared.relayURL,
		ShareID:  shared.link.ShareID,
	})
	if err != nil {
		t.Fatalf("dial M1c receiver: %v", err)
	}
	out := t.TempDir()
	sink, err := osfs.NewSink(out, osfs.SinkOptions{})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	receiver, err := share.NewReceiver(shared.link, conn.SealedManifest(), sink)
	if err != nil {
		_ = conn.Close()
		_ = sink.Close()
		t.Fatal(err)
	}
	plan, err := receiver.Plan(nil)
	if err != nil {
		_ = conn.Close()
		_ = sink.Close()
		t.Fatal(err)
	}
	writeSink := session.Sink(plan.Sink())
	if decorate != nil {
		writeSink = decorate(writeSink)
	}
	receiveSession, err := session.NewReceiveSession(
		plan.Chunks(),
		writeSink,
		receiver.Opener(),
		session.Options{MaxBlockBytes: receiver.MaxBlockBytes()},
	)
	if err != nil {
		_ = conn.Close()
		_ = sink.Close()
		t.Fatal(err)
	}
	result := &m1cReceiver{
		conn:    conn,
		sink:    sink,
		plan:    plan,
		session: receiveSession,
		outTree: filepath.Join(out, "tree"),
	}
	var channel session.FrameChannel
	switch path {
	case m1cRelayPath:
		channel = conn.Channel()
	case m1cPeerPath:
		signaling, err := connectivity.NewRelaySignaling(conn.Channel())
		if err != nil {
			result.close()
			t.Fatal(err)
		}
		peer, err := connectivity.NewPionChannelFactory(
			connectivity.DefaultPionConfiguration(),
		).Offer(ctx, signaling)
		if err != nil {
			result.close()
			t.Fatalf("negotiate M1c P2P receiver: %v", err)
		}
		result.peer = peer
		channel = peer
	default:
		result.close()
		t.Fatalf("unknown M1c path %q", path)
	}
	result.probe = newPathProbe(channel)
	if err := receiveSession.AddChannel(result.probe); err != nil {
		result.close()
		t.Fatal(err)
	}
	return result
}

func (r *m1cReceiver) close() {
	if r.probe != nil {
		_ = r.probe.Close()
	}
	if r.peer != nil {
		_ = r.peer.Close()
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if r.sink != nil {
		_ = r.sink.Close()
	}
}

func runM1CReceiver(receiver *m1cReceiver, ctx context.Context) <-chan error {
	result := make(chan error, 1)
	go func() { result <- receiver.session.Run(ctx) }()
	return result
}

func waitM1CReceiver(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(getTimeout):
		t.Fatal("M1c receiver did not settle")
		return nil
	}
}

type gatedSink struct {
	session.Sink
	entered chan struct{}
	result  chan error
	once    sync.Once
}

func newGatedSink(sink session.Sink) *gatedSink {
	return &gatedSink{
		Sink:    sink,
		entered: make(chan struct{}),
		result:  make(chan error, 1),
	}
}

func (s *gatedSink) WriteBlock(index uint64, plaintext []byte) error {
	s.once.Do(func() { close(s.entered) })
	if err := <-s.result; err != nil {
		return err
	}
	return s.Sink.WriteBlock(index, plaintext)
}

func (s *gatedSink) fail(err error) { s.result <- err }

func TestM1cPathProbePreservesBackpressureAndAcceptedFrameMetrics(t *testing.T) {
	t.Run("outbound request", func(t *testing.T) {
		sendFailure := errors.New("reject probe request")
		channel := newPathProbeTestChannel()
		channel.sendErr = sendFailure
		probe := newPathProbe(channel)
		t.Cleanup(func() { _ = probe.Close() })

		request := session.Frame{session.FrameRequest}
		if err := probe.Send(t.Context(), request); !errors.Is(err, sendFailure) {
			t.Fatalf("Send failure = %v, want %v", err, sendFailure)
		}
		if got := probe.requestFrames.Load(); got != 0 {
			t.Fatalf("rejected request count = %d, want 0", got)
		}
		channel.sendErr = nil
		if err := probe.Send(t.Context(), request); err != nil {
			t.Fatalf("Send accepted request: %v", err)
		}
		if got := probe.requestFrames.Load(); got != 1 {
			t.Fatalf("accepted request count = %d, want 1", got)
		}
	})

	inboundCases := []struct {
		name      string
		frameType byte
		count     func(*pathProbe) int64
	}{
		{name: "block", frameType: session.FrameBlock, count: func(p *pathProbe) int64 {
			return p.blockFrames.Load()
		}},
		{name: "error", frameType: session.FrameError, count: func(p *pathProbe) int64 {
			return p.errorFrames.Load()
		}},
	}
	for _, testCase := range inboundCases {
		t.Run(testCase.name, func(t *testing.T) {
			channel := newPathProbeTestChannel()
			probe := newPathProbe(channel)
			t.Cleanup(func() { _ = probe.Close() })
			if got := cap(probe.recv); got != 0 {
				t.Fatalf("probe receive capacity = %d, want unbuffered", got)
			}

			forwarded := make(chan struct{})
			go func() {
				channel.recv <- session.Frame{testCase.frameType}
				close(forwarded)
			}()
			select {
			case <-forwarded:
			case <-time.After(getTimeout):
				t.Fatal("probe did not accept the underlying frame")
			}
			if got := testCase.count(probe); got != 0 {
				t.Fatalf("unforwarded %s count = %d, want 0", testCase.name, got)
			}
			frame := <-probe.Recv()
			if len(frame) != 1 || frame[0] != testCase.frameType {
				t.Fatalf("forwarded frame = %v, want type %d", frame, testCase.frameType)
			}
			if err := probe.Close(); err != nil {
				t.Fatalf("close probe: %v", err)
			}
			if got := testCase.count(probe); got != 1 {
				t.Fatalf("forwarded %s count = %d, want 1", testCase.name, got)
			}
		})
	}
}

func TestM1cTerminalDriftUsesRelayAndP2PTerminalPaths(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	for _, path := range []m1cPath{m1cRelayPath, m1cPeerPath} {
		t.Run(string(path), func(t *testing.T) {
			payload := make([]byte, 3*e2eBlockSizeInt)
			for index := range payload {
				payload[index] = byte(index*37 + 17)
			}
			sourceRoot := writeTree(t, treeSpec{files: map[string][]byte{"drift.bin": payload}})
			shared := startM1CShare(t, sourceRoot)
			target := filepath.Join(sourceRoot, "drift.bin")
			if err := os.Chtimes(target, time.Time{}, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), getTimeout)
			defer cancel()
			receiver := prepareM1CReceiver(t, ctx, shared, path, nil)
			defer receiver.close()
			runErr := receiver.session.Run(ctx)
			var terminal *session.Error
			if !errors.As(runErr, &terminal) || terminal.Code != session.ErrCodeBlockRead {
				t.Fatalf("%s terminal = %v, want ErrCodeBlockRead", path, runErr)
			}
			if receiver.probe.requestFrames.Load() == 0 {
				t.Fatalf("%s path carried no request", path)
			}
			if receiver.probe.errorFrames.Load() == 0 {
				t.Fatalf("%s path carried no terminal error frame", path)
			}
			if senderErr := shared.wait(t); !errors.Is(senderErr, osfs.ErrDrift) {
				t.Fatalf("share sender error = %v, want source drift", senderErr)
			}
		})
	}
}

func TestM1cConcurrentReceiverFailureDoesNotTearDownSiblings(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	payload := make([]byte, 2<<20)
	for index := range payload {
		payload[index] = byte(index*41 + 23)
	}
	sourceRoot := writeTree(t, treeSpec{files: map[string][]byte{"fanout.bin": payload}})
	shared := startM1CShare(t, sourceRoot)
	ctx, cancel := context.WithTimeout(t.Context(), getTimeout)
	defer cancel()

	var slowGate *gatedSink
	slow := prepareM1CReceiver(t, ctx, shared, m1cPeerPath, func(sink session.Sink) session.Sink {
		slowGate = newGatedSink(sink)
		return slowGate
	})
	defer slow.close()
	fast := prepareM1CReceiver(t, ctx, shared, m1cPeerPath, nil)
	defer fast.close()
	slowResult := runM1CReceiver(slow, ctx)
	fastResult := runM1CReceiver(fast, ctx)

	select {
	case <-slowGate.entered:
	case <-time.After(procIOTimeout):
		t.Fatal("slow receiver did not reach its real sink boundary")
	}
	if err := waitM1CReceiver(t, fastResult); err != nil {
		t.Fatalf("fast sibling failed while another receiver was blocked: %v", err)
	}
	if err := fast.plan.Finalize(); err != nil {
		t.Fatalf("finalize fast sibling: %v", err)
	}
	assertTreeEqual(t, sourceRoot, fast.outTree)

	errSlowReceiver := errors.New("injected receiver-local sink failure")
	slowGate.fail(errSlowReceiver)
	if err := waitM1CReceiver(t, slowResult); !errors.Is(err, errSlowReceiver) {
		t.Fatalf("slow receiver error = %v, want injected sink failure", err)
	}
	slow.close()
	fast.close()

	// A fresh receiver after the failed sibling proves the share-level accept loop
	// remained alive rather than merely letting an already-finished sibling escape.
	afterFailure := prepareM1CReceiver(t, ctx, shared, m1cPeerPath, nil)
	defer afterFailure.close()
	if err := afterFailure.session.Run(ctx); err != nil {
		t.Fatalf("receiver after sibling failure: %v", err)
	}
	if err := afterFailure.plan.Finalize(); err != nil {
		t.Fatalf("finalize receiver after sibling failure: %v", err)
	}
	assertTreeEqual(t, sourceRoot, afterFailure.outTree)

	if fast.probe.blockFrames.Load() == 0 || afterFailure.probe.blockFrames.Load() == 0 {
		t.Fatal("successful siblings did not receive blocks over their P2P paths")
	}
	if senderErr := shared.stop(t); !errors.Is(senderErr, context.Canceled) {
		t.Fatalf("share sender shutdown error = %v, want context cancellation", senderErr)
	}
}
