package pionpeer

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

const turnCancelTestTimeout = 2 * time.Second

func startTURNGathering(t *testing.T, conn *Connection) {
	t.Helper()
	if _, err := conn.CreateDataChannel("turn-cancel-test", nil); err != nil {
		t.Fatalf("create data channel: %v", err)
	}
	offer, err := conn.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := conn.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
}

func closeWithin(t *testing.T, conn *Connection) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- conn.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close TURN peer connection: %v", err)
		}
	case <-time.After(turnCancelTestTimeout):
		t.Fatal("TURN peer connection Close blocked waiting for gathering")
	}
}

func TestTURNConnectionCloseAbortsBlockedTCPDialAndLookup(t *testing.T) {
	t.Run("close aborts blocked TCP dial", func(t *testing.T) {
		relay, err := iceconfig.NewTURNRelay([]string{"turn:192.0.2.1:3478?transport=tcp"}, "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		dialStarted := make(chan struct{}, 2)
		dialErr := make(chan error, 2)
		hooks := defaultTURNDialHooks()
		hooks.dialContext = func(ctx context.Context, _ string, _ net.Addr, _ string) (net.Conn, error) {
			dialStarted <- struct{}{}
			<-ctx.Done()
			err := context.Cause(ctx)
			dialErr <- err
			return nil, err
		}
		conn, err := newTURNPeerConnection(context.Background(), pion.Configuration{}, relay, AttemptConfig{}, hooks)
		if err != nil {
			t.Fatal(err)
		}
		startTURNGathering(t, conn)
		select {
		case <-dialStarted:
		case <-time.After(turnCancelTestTimeout):
			_ = conn.Close()
			t.Fatal("TURN gatherer never started TCP dial")
		}
		closeWithin(t, conn)
		select {
		case err := <-dialErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("dial exit err = %v, want context.Canceled", err)
			}
		case <-time.After(turnCancelTestTimeout):
			t.Fatal("blocked TCP dial did not observe Close cancellation")
		}
	})

	t.Run("attempt context cancellation aborts blocked TLS dial", func(t *testing.T) {
		relay, err := iceconfig.NewTURNRelay([]string{"turns:192.0.2.1:443?transport=tcp"}, "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		attemptCtx, cancelAttempt := context.WithCancelCause(t.Context())
		defer cancelAttempt(nil)
		dialStarted := make(chan struct{}, 2)
		dialErr := make(chan error, 2)
		hooks := defaultTURNDialHooks()
		hooks.dialContext = func(ctx context.Context, _ string, _ net.Addr, _ string) (net.Conn, error) {
			dialStarted <- struct{}{}
			<-ctx.Done()
			err := context.Cause(ctx)
			dialErr <- err
			return nil, err
		}
		conn, err := newTURNPeerConnection(attemptCtx, pion.Configuration{}, relay, AttemptConfig{}, hooks)
		if err != nil {
			t.Fatal(err)
		}
		defer closeWithin(t, conn)
		startTURNGathering(t, conn)
		select {
		case <-dialStarted:
		case <-time.After(turnCancelTestTimeout):
			t.Fatal("TURN gatherer never started TLS TCP dial")
		}
		cancelCause := errors.New("attempt timed out")
		cancelAttempt(cancelCause)
		select {
		case err := <-dialErr:
			if !errors.Is(err, cancelCause) {
				t.Fatalf("dial exit err = %v, want %v", err, cancelCause)
			}
		case <-time.After(turnCancelTestTimeout):
			t.Fatal("blocked TLS TCP dial did not observe attempt cancellation")
		}
	})

	t.Run("close aborts blocked DNS lookup", func(t *testing.T) {
		relay, err := iceconfig.NewTURNRelay([]string{"turns:turn.invalid:443?transport=tcp"}, "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		lookupStarted := make(chan struct{}, 4)
		lookupErr := make(chan error, 4)
		hooks := defaultTURNDialHooks()
		hooks.lookupIP = func(ctx context.Context, _, _ string) ([]net.IP, error) {
			lookupStarted <- struct{}{}
			<-ctx.Done()
			err := context.Cause(ctx)
			lookupErr <- err
			return nil, err
		}
		conn, err := newTURNPeerConnection(context.Background(), pion.Configuration{}, relay, AttemptConfig{}, hooks)
		if err != nil {
			t.Fatal(err)
		}
		startTURNGathering(t, conn)
		select {
		case <-lookupStarted:
		case <-time.After(turnCancelTestTimeout):
			_ = conn.Close()
			t.Fatal("TURN gatherer never started DNS lookup")
		}
		closeWithin(t, conn)
		select {
		case err := <-lookupErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lookup exit err = %v, want context.Canceled", err)
			}
		case <-time.After(turnCancelTestTimeout):
			t.Fatal("blocked DNS lookup did not observe Close cancellation")
		}
	})

	t.Run("already canceled context fails fast", func(t *testing.T) {
		relay, err := iceconfig.NewTURNRelay([]string{"turn:192.0.2.1:3478?transport=tcp"}, "user", "pass")
		if err != nil {
			t.Fatal(err)
		}
		cause := errors.New("pre-canceled")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(cause)
		if conn, err := NewTURNPeerConnection(ctx, pion.Configuration{}, relay, AttemptConfig{}); !errors.Is(err, cause) {
			if conn != nil {
				_ = conn.Close()
			}
			t.Fatalf("err = %v, want %v", err, cause)
		}
	})
}

func TestTURNDialNetLiveTCPConnectionSurvivesContextCancel(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	dialNet, err := newTURNDialNet(ctx, defaultTURNDialHooks())
	if err != nil {
		t.Fatal(err)
	}

	addr := listener.Addr().String()
	tcpAddr, err := dialNet.ResolveTCPAddr("tcp4", addr)
	if err != nil || !tcpAddr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("ResolveTCPAddr(tcp4) = %v, %v", tcpAddr, err)
	}
	if _, err := dialNet.ResolveTCPAddr("tcp6", addr); err == nil {
		t.Fatal("ResolveTCPAddr(tcp6) accepted an IPv4 literal")
	}
	if udpAddr, err := dialNet.ResolveUDPAddr("udp4", addr); err != nil || udpAddr.Port != tcpAddr.Port {
		t.Fatalf("ResolveUDPAddr(udp4) = %v, %v", udpAddr, err)
	}
	if udp6Addr, err := dialNet.ResolveUDPAddr("udp6", "[::1]:3478"); err != nil || !udp6Addr.IP.Equal(net.IPv6loopback) || udp6Addr.Port != 3478 {
		t.Fatalf("ResolveUDPAddr(udp6) = %v, %v", udp6Addr, err)
	}

	conn, err := dialNet.DialTCP("tcp4", nil, tcpAddr)
	if err != nil {
		t.Fatalf("DialTCP failed: %v", err)
	}
	defer conn.Close()

	// Canceling the negotiation context after dialing must not disrupt an established TURN socket.
	cancel()
	const payload = "established-turn-tcp"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write after dial context cancel: %v", err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read after dial context cancel: %v", err)
	}
	if string(reply) != payload {
		t.Fatalf("echo = %q, want %q", reply, payload)
	}

	if extra, err := dialNet.DialTCP("tcp4", nil, tcpAddr); !errors.Is(err, context.Canceled) {
		if extra != nil {
			_ = extra.Close()
		}
		t.Fatalf("post-cancel DialTCP err = %v, want context.Canceled", err)
	}
}
