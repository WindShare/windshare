package stunonly

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func TestBindingOnlyPreservesTransactionAndActualPort(t *testing.T) {
	for _, ip := range []string{"192.0.2.25", "2001:db8::25"} {
		request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		response, err := bindingResponse(request.Raw, &net.UDPAddr{IP: net.ParseIP(ip), Port: 54321})
		if err != nil {
			t.Fatal(err)
		}
		message := &stun.Message{Raw: response}
		if err = message.Decode(); err != nil {
			t.Fatal(err)
		}
		var address stun.XORMappedAddress
		if err = address.GetFrom(message); err != nil {
			t.Fatal(err)
		}
		if address.IP.String() != ip || address.Port != 54321 || message.TransactionID != request.TransactionID {
			t.Fatal(address)
		}
		if err = stun.Fingerprint.Check(message); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range [][]byte{[]byte("bad"), stun.MustBuild(stun.TransactionID, stun.NewType(stun.MethodAllocate, stun.ClassRequest)).Raw, append(stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw, 0)} {
		if _, err := bindingResponse(data, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 4}); err == nil {
			t.Fatal("nonbinding accepted")
		}
	}
}
func TestListenerLifecycleRateLimitsAndHealth(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(conn, Config{RequestsPerSecond: 10, SourceRequestsPerSecond: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client, err := net.Dial("udp4", conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err = client.Write(request.Raw); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 100)
	if _, err = client.Read(response); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write(request.Raw); err != nil {
		t.Fatal(err)
	}
	// A response proves the service loop is active; metrics remain race-safe.
	handler := Handler([]ListenerStatus{{ID: "udp-0", Server: server}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), "windshare_stun_responded_total") {
		t.Fatal(recorder.Body.String())
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if server.Metrics().Healthy {
		t.Fatal("retired healthy")
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatal(recorder.Code)
	}
}
func TestRateLimitCapacityWindowAndClock(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newRateLimiter(Config{RequestsPerSecond: 3, SourceRequestsPerSecond: 2, MaximumSources: 2})
	for request := range 2 {
		if !limiter.allow("a", now) {
			t.Fatalf("source request %d rejected before its limit", request+1)
		}
	}
	if limiter.allow("a", now) {
		t.Fatal("source request accepted beyond its limit")
	}
	if !limiter.allow("b", now) || limiter.allow("b", now) {
		t.Fatal("global")
	}
	now = now.Add(time.Second)
	if !limiter.allow("b", now) || !limiter.allow("c", now) || limiter.allow("d", now) {
		t.Fatal("source cardinality")
	}
	if !limiter.allow("d", now.Add(-time.Second)) {
		t.Fatal("clock rollback")
	}
}

type scriptedPacket struct {
	data   []byte
	remote net.Addr
	err    error
}
type scriptedConn struct {
	packets    []scriptedPacket
	writeError error
}

func (s *scriptedConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	if len(s.packets) == 0 {
		return 0, nil, io.EOF
	}
	packet := s.packets[0]
	s.packets = s.packets[1:]
	return copy(buffer, packet.data), packet.remote, packet.err
}
func (s *scriptedConn) WriteTo(data []byte, _ net.Addr) (int, error) { return len(data), s.writeError }
func (s *scriptedConn) Close() error                                 { return nil }
func (s *scriptedConn) LocalAddr() net.Addr                          { return &net.UDPAddr{} }
func (s *scriptedConn) SetDeadline(time.Time) error                  { return nil }
func (s *scriptedConn) SetReadDeadline(time.Time) error              { return nil }
func (s *scriptedConn) SetWriteDeadline(time.Time) error             { return nil }
func TestInvalidPacketsAndWriteFailuresStayWithinListener(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}
	conn := &scriptedConn{writeError: io.ErrClosedPipe, packets: []scriptedPacket{
		{data: []byte("bad"), remote: addr},
		{data: make([]byte, MaximumDatagramBytes+1), remote: addr},
		{data: []byte("bad"), remote: &net.TCPAddr{}},
		{data: stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw, remote: addr},
		{data: stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw, remote: addr},
	}}
	server, err := New(conn, Config{RequestsPerSecond: 2, SourceRequestsPerSecond: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Serve(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	metrics := server.Metrics()
	if metrics.Invalid != 3 || metrics.WriteErrors != 1 || metrics.Limited != 1 || metrics.Received != 5 {
		t.Fatal(metrics)
	}
	if _, err = New(nil, Config{}); err == nil {
		t.Fatal("nil accepted")
	}
	if _, err = New(conn, Config{MaximumSources: -1}); err == nil {
		t.Fatal("invalid accepted")
	}
}
