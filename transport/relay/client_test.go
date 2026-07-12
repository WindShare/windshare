package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// waitUntil 轮询直至条件成立;测试专用,失败交由调用方断言现场。
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestWaitBackoff(t *testing.T) {
	delay := 10 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)
	if !waitBackoff(context.Background(), nil, deadline, &delay, 25*time.Millisecond) {
		t.Fatal("expected first wait to succeed")
	}
	if delay != 20*time.Millisecond {
		t.Fatalf("delay after first wait = %v, want 20ms", delay)
	}
	if !waitBackoff(context.Background(), nil, deadline, &delay, 25*time.Millisecond) {
		t.Fatal("expected second wait to succeed")
	}
	if delay != 25*time.Millisecond {
		t.Fatalf("delay should cap at max, got %v", delay)
	}

	if waitBackoff(context.Background(), nil, time.Now().Add(-time.Second), &delay, time.Second) {
		t.Fatal("expired window must fail immediately")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if waitBackoff(canceled, nil, time.Now().Add(time.Hour), &delay, time.Hour) {
		t.Fatal("canceled ctx must abort the wait")
	}

	stop := make(chan struct{})
	close(stop)
	if waitBackoff(context.Background(), stop, time.Now().Add(time.Hour), &delay, time.Hour) {
		t.Fatal("closed stop channel must abort the wait")
	}
}

func TestMustEncodePanicsOnBadMessage(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for invalid message")
		}
	}()
	mustEncode(protocol.NewBye("!!!"))
}

type discardWriter struct{}

func (discardWriter) WriteText([]byte) error   { return nil }
func (discardWriter) WriteBinary([]byte) error { return nil }

type firstWriteGate struct {
	once        sync.Once
	releaseOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func newFirstWriteGate() *firstWriteGate {
	return &firstWriteGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *firstWriteGate) WriteText([]byte) error { return nil }

func (w *firstWriteGate) WriteBinary([]byte) error {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return nil
}

func (w *firstWriteGate) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func deadEndLink() (*link, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &link{ctx: ctx, cancel: cancel}
	l.pump = forward.NewPump(discardWriter{}, forward.Options{})
	return l, cancel
}

func TestChannelRejectsSendAfterContextOrTerminal(t *testing.T) {
	l, cancel := deadEndLink()
	defer func() {
		cancel()
		l.pump.Close()
		<-l.pump.Done()
	}()
	ch := newChannel(protocol.SessionID{1}, l)

	canceled, ccancel := context.WithCancel(context.Background())
	ccancel()
	if err := ch.Send(canceled, session.Frame{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("send with canceled ctx = %v, want context.Canceled", err)
	}

	ch.shut(nil)
	if err := ch.Send(context.Background(), session.Frame{1}); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send after terminal = %v, want ErrChannelClosed", err)
	}
	if _, ok := <-ch.Recv(); ok {
		t.Fatal("recv must close with channel state")
	}

	ch2 := newChannel(protocol.SessionID{2}, l)
	cancel()
	if err := ch2.Send(context.Background(), session.Frame{1}); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on dead link = %v, want ErrChannelClosed", err)
	}
}

func TestChannelSendBackpressuresInsteadOfFailingOnQueueSaturation(t *testing.T) {
	w := newFirstWriteGate()
	linkCtx, cancel := context.WithCancel(context.Background())
	l := &link{ctx: linkCtx, cancel: cancel}
	l.pump = forward.NewPump(w, forward.Options{SessionQueueFrames: 1})
	t.Cleanup(func() {
		w.unblock()
		cancel()
		l.pump.Close()
		<-l.pump.Done()
	})
	ch := newChannel(protocol.SessionID{3}, l)

	if err := ch.Send(context.Background(), session.Frame{1}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not receive first frame")
	}
	if err := ch.Send(context.Background(), session.Frame{2}); err != nil {
		t.Fatalf("queued Send: %v", err)
	}
	third := make(chan error, 1)
	go func() { third <- ch.Send(context.Background(), session.Frame{3}) }()
	select {
	case err := <-third:
		t.Fatalf("saturated Send returned before capacity was available: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	w.unblock()
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("backpressured Send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backpressured Send did not resume")
	}
	idleCtx, idleCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer idleCancel()
	if !l.pump.WaitIdle(idleCtx) {
		t.Fatal("pump did not drain after backpressure was released")
	}
}

func TestChannelCloseWaitsForAcceptedTerminal(t *testing.T) {
	w := newFirstWriteGate()
	linkCtx, cancel := context.WithCancel(context.Background())
	l := &link{ctx: linkCtx, cancel: cancel}
	l.pump = forward.NewPump(w, forward.Options{})
	t.Cleanup(func() {
		w.unblock()
		cancel()
		l.pump.Close()
		<-l.pump.Done()
	})
	ch := newChannel(protocol.SessionID{4}, l)

	terminalResult := make(chan error, 1)
	go func() { terminalResult <- ch.SendTerminal(context.Background(), session.Frame{1}) }()
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not receive terminal frame")
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- ch.Close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("Close overtook the accepted terminal: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	w.unblock()
	if err := <-terminalResult; err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("concurrent Close: %v", err)
	}
	if ch.State() != session.Closed {
		t.Fatalf("channel state = %v, want Closed", ch.State())
	}
}

func TestChannelClosesWhenAcceptedTerminalWaitIsCanceled(t *testing.T) {
	w := newFirstWriteGate()
	linkCtx, cancelLink := context.WithCancel(context.Background())
	l := &link{ctx: linkCtx, cancel: cancelLink}
	l.pump = forward.NewPump(w, forward.Options{})
	t.Cleanup(func() {
		w.unblock()
		cancelLink()
		l.pump.Close()
		<-l.pump.Done()
	})
	ch := newChannel(protocol.SessionID{5}, l)

	terminalCtx, cancelTerminal := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- ch.SendTerminal(terminalCtx, session.Frame{1}) }()
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not receive terminal frame")
	}
	cancelTerminal()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("SendTerminal after cancellation = %v, want context.Canceled", err)
	}
	if ch.State() != session.Closed {
		t.Fatalf("channel state = %v, want Closed", ch.State())
	}
	if !errors.Is(ch.Err(), context.Canceled) {
		t.Fatalf("channel error = %v, want context.Canceled", ch.Err())
	}
	w.unblock()
	idleCtx, idleCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer idleCancel()
	if !l.pump.WaitIdle(idleCtx) {
		t.Fatal("accepted terminal did not drain after caller cancellation")
	}
}

func TestDialWSRejectsBadInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := dialWS(ctx, "ftp://relay.example", "c2hhcmU", nil); err == nil {
		t.Fatal("expected scheme error")
	}
	if _, err := dialWS(ctx, "http://relay.example", "bad!id", nil); err == nil {
		t.Fatal("expected shareId error")
	}
	if _, err := dialWS(ctx, "http://relay\x00.example", "c2hhcmU", nil); err == nil {
		t.Fatal("expected URL parse error")
	}
}

func TestDialWSErrorsDoNotExposeRelayURLSecrets(t *testing.T) {
	const (
		querySecret    = "VERYSECRET"
		fragmentSecret = "FRAGMENTSECRET"
		pathSecret     = "PATHSECRET"
	)
	dialFailure := errors.New("dial failed")
	hc := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		// Model net/http and proxy errors that retain the complete request URL.
		return nil, fmt.Errorf("request %s: %w", request.URL, dialFailure)
	})}

	_, err := dialWS(t.Context(), "https://user:password@relay.example/"+pathSecret+"?token="+querySecret+"#"+fragmentSecret, "c2hhcmU", hc)
	if !errors.Is(err, dialFailure) {
		t.Fatalf("dial error = %v, want preserved transport cause", err)
	}
	for _, secret := range []string{"user", "password", pathSecret, querySecret, fragmentSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("dial diagnostic exposed relay URL secret %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "wss://relay.example") {
		t.Fatalf("dial diagnostic lost the public endpoint: %v", err)
	}
}

func TestPublicRelayEndpointBoundsUTF8Diagnostic(t *testing.T) {
	u := &url.URL{
		Scheme:   "https",
		Host:     strings.Repeat("界", maxRelayEndpointDiagnosticBytes) + ".example",
		Path:     "/path-secret",
		RawQuery: "token=secret",
		Fragment: "fragment",
	}
	got := publicRelayEndpoint(u)
	if !utf8.ValidString(got) {
		t.Fatalf("endpoint diagnostic is not valid UTF-8: %q", got)
	}
	if len(got) > maxRelayEndpointDiagnosticBytes+len("…") {
		t.Fatalf("endpoint diagnostic has %d bytes, limit %d", len(got), maxRelayEndpointDiagnosticBytes+len("…"))
	}
	if strings.Contains(got, "path-secret") || strings.Contains(got, "secret") || strings.Contains(got, "fragment") {
		t.Fatalf("endpoint diagnostic exposed query or fragment: %q", got)
	}
}
