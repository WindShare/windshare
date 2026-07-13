package connectivity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

type sourceResult struct {
	receiver AcceptedReceiver
	err      error
}

type scriptedReceiverSource struct {
	results   chan sourceResult
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	onClose   func()
}

func newScriptedReceiverSource() *scriptedReceiverSource {
	return &scriptedReceiverSource{results: make(chan sourceResult, 8), closed: make(chan struct{})}
}

func (s *scriptedReceiverSource) Accept(ctx context.Context) (AcceptedReceiver, error) {
	select {
	case <-ctx.Done():
		return AcceptedReceiver{}, ctx.Err()
	case <-s.closed:
		return AcceptedReceiver{}, io.EOF
	case result := <-s.results:
		return result.receiver, result.err
	}
}

func (s *scriptedReceiverSource) Close() error {
	s.closeOnce.Do(func() {
		if s.onClose != nil {
			s.onClose()
		}
		close(s.closed)
	})
	return s.closeErr
}

type closeObservedChannel struct {
	*fakePeerChannel
	closeOnce sync.Once
	onClose   func()
}

func (c *closeObservedChannel) Close() error {
	c.closeOnce.Do(c.onClose)
	return c.fakePeerChannel.Close()
}

func TestShareSenderSlowReceiverDoesNotBlockSibling(t *testing.T) {
	source := newScriptedReceiverSource()
	slowRelay := newFakePeerChannel()
	healthyRelay := newFakePeerChannel()
	slowGate := slowRelay.blockSends()
	slowPeer := newFakePeerChannel()
	healthyPeer := newFakePeerChannel()
	slowPeer.closeWithError(nil)
	healthyPeer.closeWithError(nil)
	peers := map[string]PeerChannel{"slow": slowPeer, "healthy": healthyPeer}
	started := make(chan string, 2)
	sender, err := NewShareSender(
		source,
		answerFactoryFunc(func(_ context.Context, signaling Signaling) (PeerChannel, error) {
			return peers[signaling.(*inertSignaling).id], nil
		}),
		&recordingStore{block: []byte("fan-out")},
		&recordingSealer{},
		ShareSenderOptions{OnReceiverStart: func(receiver AcceptedReceiver) { started <- receiver.ID }},
	)
	if err != nil {
		t.Fatal(err)
	}
	source.results <- sourceResult{receiver: AcceptedReceiver{ID: "slow", Channel: slowRelay, Signaling: &inertSignaling{id: "slow"}}}
	source.results <- sourceResult{receiver: AcceptedReceiver{ID: "healthy", Channel: healthyRelay, Signaling: &inertSignaling{id: "healthy"}}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- sender.Run(ctx) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(orchestrationTestTimeout):
			t.Fatal("share sender did not start both receivers")
		}
	}
	request := mustRequest(t)
	if err := slowRelay.deliver(request); err != nil {
		t.Fatal(err)
	}
	if err := healthyRelay.deliver(request); err != nil {
		t.Fatal(err)
	}
	assertBlockSent(t, healthyRelay)
	select {
	case <-slowRelay.sent:
		t.Fatal("slow receiver unexpectedly passed its blocked transport")
	default:
	}
	cancel()
	close(slowGate)
	if err := waitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("ShareSender.Run error = %v", err)
	}
}

func TestShareSenderFirstFatalCancelsEverySibling(t *testing.T) {
	wantErr := errors.New("shared source failed")
	source := newScriptedReceiverSource()
	fatalRelay := newFakePeerChannel()
	idleRelay := newFakePeerChannel()
	fatalPeer := newFakePeerChannel()
	idlePeer := newFakePeerChannel()
	peers := map[string]PeerChannel{"fatal": fatalPeer, "idle": idlePeer}
	started := make(chan string, 2)
	sender, err := NewShareSender(
		source,
		answerFactoryFunc(func(_ context.Context, signaling Signaling) (PeerChannel, error) {
			return peers[signaling.(*inertSignaling).id], nil
		}),
		&recordingStore{err: wantErr},
		&recordingSealer{},
		ShareSenderOptions{
			ClassifyError: func(err error) SendErrorDisposition {
				if errors.Is(err, wantErr) {
					return SendShareFatal
				}
				return SendPathEnded
			},
			OnReceiverStart: func(receiver AcceptedReceiver) { started <- receiver.ID },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	source.results <- sourceResult{receiver: AcceptedReceiver{ID: "fatal", Channel: fatalRelay, Signaling: &inertSignaling{id: "fatal"}}}
	source.results <- sourceResult{receiver: AcceptedReceiver{ID: "idle", Channel: idleRelay, Signaling: &inertSignaling{id: "idle"}}}
	result := make(chan error, 1)
	go func() { result <- sender.Run(t.Context()) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(orchestrationTestTimeout):
			t.Fatal("share sender did not start both receivers")
		}
	}
	if err := fatalRelay.deliver(mustRequest(t)); err != nil {
		t.Fatal(err)
	}
	if err := waitError(t, result); !errors.Is(err, wantErr) {
		t.Fatalf("ShareSender.Run error = %v", err)
	}
	for name, channel := range map[string]*fakePeerChannel{
		"fatal peer": fatalPeer,
		"idle relay": idleRelay,
		"idle peer":  idlePeer,
	} {
		select {
		case <-channel.Done():
		case <-time.After(orchestrationTestTimeout):
			t.Fatalf("%s was not canceled after first share-fatal outcome", name)
		}
	}
}

func TestShareSenderCancellationWithdrawsSourceBeforeSessions(t *testing.T) {
	events := make(chan string, 2)
	source := newScriptedReceiverSource()
	source.onClose = func() { events <- "source" }
	relayChannel := &closeObservedChannel{
		fakePeerChannel: newFakePeerChannel(),
		onClose:         func() { events <- "session" },
	}
	started := make(chan struct{}, 1)
	sender, err := NewShareSender(
		source,
		answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) {
			return newFakePeerChannel(), nil
		}),
		&recordingStore{block: []byte("unused")},
		&recordingSealer{},
		ShareSenderOptions{OnReceiverStart: func(AcceptedReceiver) { started <- struct{}{} }},
	)
	if err != nil {
		t.Fatal(err)
	}
	source.results <- sourceResult{receiver: AcceptedReceiver{
		ID: "ordered", Channel: relayChannel, Signaling: &inertSignaling{id: "ordered"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- sender.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(orchestrationTestTimeout):
		t.Fatal("share sender did not start the receiver")
	}

	cancel()
	if err := waitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("ShareSender.Run error = %v, want context cancellation", err)
	}
	for index, want := range []string{"source", "session"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("shutdown event %d = %q, want %q", index, got, want)
			}
		case <-time.After(orchestrationTestTimeout):
			t.Fatalf("shutdown event %d (%s) was not observed", index, want)
		}
	}
}

func TestClassifySendErrorPreservesShareFatalBranches(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want SendErrorDisposition
	}{
		{name: "nil", want: SendPathEnded},
		{name: "context cancellation", err: context.Canceled, want: SendPathEnded},
		{name: "session closed", err: session.ErrSessionClosed, want: SendPathEnded},
		{name: "peer violation", err: fmt.Errorf("peer: %w", session.ErrPeerViolation), want: SendPathEnded},
		{name: "peer terminal", err: &session.Error{Code: session.ErrCodeBadRequest, Msg: "bad"}, want: SendPathEnded},
		{name: "WebRTC transport", err: fmt.Errorf("%w: socket", transportwebrtc.ErrTransport), want: SendPathEnded},
		{name: "joined path outcomes", err: errors.Join(context.Canceled, transportwebrtc.ErrRemoteClosed), want: SendPathEnded},
		{name: "peer cause with terminal delivery", err: errors.Join(session.ErrPeerViolation, fmt.Errorf("%w: closed", session.ErrTerminalDelivery)), want: SendPathEnded},
		// SendSession.handle wraps the decode detail with a second %w, so the
		// sentinel must win over the generic join aggregation: one receiver's
		// malformed frame ends that path, never the share.
		{name: "peer violation wrapping decode detail", err: fmt.Errorf("%w: %w", session.ErrPeerViolation, errors.New("session: unknown frame type")), want: SendPathEnded},
		{name: "peer violation wrapping decode detail with terminal delivery", err: errors.Join(fmt.Errorf("%w: %w", session.ErrPeerViolation, errors.New("session: unknown frame type")), fmt.Errorf("%w: closed", session.ErrTerminalDelivery)), want: SendPathEnded},
		{name: "drift", err: fmt.Errorf("read: %w", osfs.ErrDrift), want: SendShareFatal},
		{name: "drift with terminal delivery", err: errors.Join(osfs.ErrDrift, fmt.Errorf("%w: closed", session.ErrTerminalDelivery)), want: SendShareFatal},
		{name: "unknown sealer with transport diagnostic", err: errors.Join(errors.New("seal failed"), fmt.Errorf("%w: %w", session.ErrTerminalDelivery, transportwebrtc.ErrTransport)), want: SendShareFatal},
		{name: "empty terminal delivery", err: session.ErrTerminalDelivery, want: SendShareFatal},
		{name: "unknown", err: errors.New("unknown source failure"), want: SendShareFatal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifySendError(test.err); got != test.want {
				t.Fatalf("ClassifySendError(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestShareSenderSourceEndOutcomes(t *testing.T) {
	wantErr := errors.New("relay source failed")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "clean", err: io.EOF},
		{name: "failed", err: wantErr, want: ErrReceiverSourceEnded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newScriptedReceiverSource()
			source.results <- sourceResult{err: test.err}
			sender, err := NewShareSender(
				source,
				answerFactoryFunc(func(context.Context, Signaling) (PeerChannel, error) { return newFakePeerChannel(), nil }),
				&recordingStore{block: []byte("unused")},
				&recordingSealer{},
				ShareSenderOptions{},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = sender.Run(t.Context())
			if !errors.Is(err, test.want) {
				t.Fatalf("ShareSender.Run error = %v, want %v", err, test.want)
			}
			if test.want != nil && !errors.Is(err, wantErr) {
				t.Fatalf("ShareSender.Run error lost source cause: %v", err)
			}
		})
	}
}

func mustRequest(t *testing.T) []byte {
	t.Helper()
	request, err := session.EncodeRequest([]uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	return request
}
