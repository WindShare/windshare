package socketauthority

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const handoffTestTimeout = 2 * time.Second
const idleTestBudget = time.Minute

// The socket drops responses. An optional blocked write acknowledges cancellation
// but stays in flight until the test releases it, exposing the handoff window.
type idleTestSocket struct {
	written      chan struct{}
	events       chan Event
	aborted      chan struct{}
	resume       chan struct{}
	closed       chan struct{}
	closeGate    <-chan struct{}
	blockOnce    sync.Once
	writeExpired atomic.Bool
	abortOnce    sync.Once
	closeOnce    sync.Once
}

func newIdleTestSocket(block bool) *idleTestSocket {
	s := &idleTestSocket{events: make(chan Event, 32), written: make(chan struct{}, 16), aborted: make(chan struct{}), closed: make(chan struct{})}
	if block {
		s.resume = make(chan struct{})
	}
	return s
}
func (s *idleTestSocket) ReadFrom([]byte) (int, net.Addr, error) {
	<-s.closed
	return 0, nil, net.ErrClosed
}
func (s *idleTestSocket) WriteTo(p []byte, _ net.Addr) (int, error) {
	s.written <- struct{}{}
	block := false
	s.blockOnce.Do(func() { block = s.resume != nil })
	if block {
		select {
		case <-s.aborted:
		case <-s.closed:
		}
		<-s.resume
		return 0, os.ErrDeadlineExceeded
	}
	if s.writeExpired.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	return len(p), nil
}
func (s *idleTestSocket) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	if s.closeGate != nil {
		<-s.closeGate
	}
	return nil
}
func (*idleTestSocket) LocalAddr() net.Addr {
	return net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:12345"))
}
func (*idleTestSocket) SetDeadline(time.Time) error     { return nil }
func (*idleTestSocket) SetReadDeadline(time.Time) error { return nil }
func (s *idleTestSocket) SetWriteDeadline(deadline time.Time) error {
	s.writeExpired.Store(!deadline.IsZero())
	if !deadline.IsZero() {
		s.abortOnce.Do(func() { close(s.aborted) })
	}
	return nil
}

func receiveHandoff[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(handoffTestTimeout):
		t.Fatal("socket operation did not complete")
		var zero T
		return zero
	}
}
func runHandoff[T any](f func() T) <-chan T {
	done := make(chan T, 1)
	go func() { done <- f() }()
	return done
}
func idleFixture(t *testing.T, socket *idleTestSocket) (*Authority, *Lease, []netip.AddrPort) {
	t.Helper()
	a := New(Config{
		RefreshTimeout: idleTestBudget, IdleInterval: idleTestBudget,
		Observe:      func(event Event) { socket.events <- event },
		ListenPacket: func(string, string) (net.PacketConn, error) { return socket, nil },
	})
	t.Cleanup(func() { _ = a.Close() })
	lease, err := a.Acquire([16]byte{1}, 1, [16]byte{2}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	servers := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:3478")}
	if err := lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)); err != nil {
		t.Fatal(err)
	}
	receiveHandoff(t, socket.written)
	return a, lease, servers
}

type claimResult struct {
	mux     *Mux
	release func()
	err     error
}

func claimAsync(lease *Lease) <-chan claimResult {
	return runHandoff(func() claimResult {
		mux, release, err := lease.Claim()
		return claimResult{mux, release, err}
	})
}

func TestIdleClaimCancelsUnansweredSTUNAndPreservesSocket(t *testing.T) {
	socket := newIdleTestSocket(false)
	_, lease, _ := idleFixture(t, socket)
	task := lease.entry.idle
	result := receiveHandoff(t, claimAsync(lease))
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.release()
	select {
	case <-task.done:
	default:
		t.Fatal("ICE received the socket before idle work exited")
	}
	select {
	case <-socket.closed:
		t.Fatal("handoff closed the reusable socket")
	default:
	}
	conn, err := result.mux.GetConn("next", socket.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WriteTo([]byte("next attempt"), socket.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	receiveHandoff(t, socket.written)
	results := map[EventKind]string{}
	for range 3 {
		event := receiveHandoff(t, socket.events)
		if event.ProtocolSessionID != lease.SessionID() || event.PeerPathID != lease.PathID() || event.NetworkGenerationID != lease.GenerationID() || event.At.IsZero() {
			t.Fatalf("socket evidence lost attribution: %+v", event)
		}
		results[event.Kind] = event.Result
	}
	if results[STUNRefreshFinished] != "canceled" || results[SocketHandoffStarted] != "pending" || results[SocketHandoffFinished] != "completed" {
		t.Fatalf("missing cancellation and handoff evidence: %v", results)
	}
}

func TestClaimReservesOwnershipWithoutHoldingAuthorityLock(t *testing.T) {
	socket := newIdleTestSocket(true)
	_, lease, servers := idleFixture(t, socket)
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(socket.resume) }) }
	defer unblock()
	result := claimAsync(lease)
	receiveHandoff(t, socket.aborted)
	duplicate := receiveHandoff(t, claimAsync(lease))
	if duplicate.err != ErrActive {
		t.Fatalf("duplicate claim: %v", duplicate.err)
	}
	for _, action := range []func() error{
		func() error { return lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget)) },
		func() error { return lease.PrepareTCP(false) },
	} {
		if err := receiveHandoff(t, runHandoff(action)); err != ErrActive {
			t.Fatalf("work entered reserved socket: %v", err)
		}
	}
	retained := receiveHandoff(t, runHandoff(func() error {
		ref, err := lease.Retain()
		if err == nil {
			err = ref.Close()
		}
		return err
	}))
	if retained != nil {
		t.Fatal(retained)
	}
	select {
	case <-result:
		t.Fatal("claim returned before the canceled write exited")
	default:
	}
	unblock()
	claim := receiveHandoff(t, result)
	if claim.err != nil {
		t.Fatal(claim.err)
	}
	defer claim.release()
	conn, err := claim.mux.GetConn("replacement", socket.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.WriteTo([]byte("usable after cancellation"), socket.LocalAddr()); err != nil {
		t.Fatalf("canceled idle write poisoned the next owner: %v", err)
	}
}

func TestClaimRevalidatesAfterIdleCancellation(t *testing.T) {
	for _, action := range []string{"retire", "release", "release_retained", "close"} {
		t.Run(action, func(t *testing.T) {
			socket := newIdleTestSocket(true)
			a, lease, _ := idleFixture(t, socket)
			var resume sync.Once
			unblock := func() { resume.Do(func() { close(socket.resume) }) }
			defer unblock()
			result := claimAsync(lease)
			receiveHandoff(t, socket.aborted)
			want := ErrClosed
			var closing <-chan error
			var retained *Lease
			switch action {
			case "retire":
				want = ErrRetired
				receiveHandoff(t, runHandoff(func() struct{} { a.Retire(1); return struct{}{} }))
			case "release_retained":
				var err error
				retained, err = lease.Retain()
				if err != nil {
					t.Fatal(err)
				}
				if err := receiveHandoff(t, runHandoff(lease.Close)); err != nil {
					t.Fatal(err)
				}
			case "release":
				closing = runHandoff(lease.Close)
				receiveHandoff(t, socket.closed)
			case "close":
				closing = runHandoff(a.Close)
				receiveHandoff(t, socket.closed)
			}
			if err := receiveHandoff(t, runHandoff(func() error { _, err := lease.Retain(); return err })); err != want {
				t.Fatalf("admission during cancellation: %v", err)
			}
			unblock()
			claim := receiveHandoff(t, result)
			if claim.err != want || claim.mux != nil || claim.release != nil {
				t.Fatalf("invalidated claim returned %+v, want %v", claim, want)
			}
			if retained != nil {
				next := receiveHandoff(t, claimAsync(retained))
				if next.err != nil {
					t.Fatalf("canceled claimant stranded ownership: %v", next.err)
				}
				next.release()
				_ = retained.Close()
			}
			if closing != nil {
				if err := receiveHandoff(t, closing); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRepeatedIdleCannotReplaceClaimingOwner(t *testing.T) {
	socket := newIdleTestSocket(true)
	_, lease, servers := idleFixture(t, socket)
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(socket.resume) }) }
	defer unblock()
	replacement := runHandoff(func() error {
		return lease.StartIdle(context.Background(), servers, time.Now().Add(idleTestBudget))
	})
	receiveHandoff(t, socket.aborted)
	claim := claimAsync(lease)
	if event := receiveHandoff(t, socket.events); event.Kind != SocketHandoffStarted {
		t.Fatalf("claim did not reserve ownership: %+v", event)
	}
	unblock()
	result := receiveHandoff(t, claim)
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.release()
	err := receiveHandoff(t, replacement)
	if err != ErrActive {
		t.Fatal(err)
	}
}

func TestIdleReplacementHonorsCallerCancellationWhileJoining(t *testing.T) {
	socket := newIdleTestSocket(true)
	_, lease, servers := idleFixture(t, socket)
	defer close(socket.resume)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replacement := runHandoff(func() error { return lease.StartIdle(ctx, servers, time.Now().Add(idleTestBudget)) })
	receiveHandoff(t, socket.aborted)
	cancel()
	if err := receiveHandoff(t, replacement); !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement ignored caller cancellation: %v", err)
	}
}
