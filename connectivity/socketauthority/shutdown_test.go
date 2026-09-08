package socketauthority

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestClosingPathRetainsCapacityAndAllCloseCallersJoin(t *testing.T) {
	socket := newIdleTestSocket(true)
	a, lease, _ := idleFixture(t, socket)
	var resume sync.Once
	unblock := func() { resume.Do(func() { close(socket.resume) }) }
	defer unblock()
	a.mu.Lock()
	a.config.Capacity = 1
	a.mu.Unlock()
	task := lease.entry.idle
	closing := runHandoff(lease.Close)
	receiveHandoff(t, socket.closed)
	if err := receiveHandoff(t, runHandoff(func() error {
		_, err := a.Acquire([16]byte{1}, 1, [16]byte{3}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
		return err
	})); err != ErrCapacity {
		t.Fatalf("closing socket stopped counting against capacity: %v", err)
	}
	repeated := runHandoff(lease.Close)
	all := runHandoff(a.Close)
	// Network retirement must remain available while socket shutdown is joining
	// the canceled writer.
	receiveHandoff(t, runHandoff(func() struct{} {
		a.Retire(1)
		return struct{}{}
	}))
	select {
	case <-closing:
		t.Fatal("lease close returned with idle work in flight")
	case <-repeated:
		t.Fatal("repeated close skipped the worker join")
	default:
	}
	unblock()
	for _, result := range []<-chan error{closing, repeated, all} {
		if err := receiveHandoff(t, result); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-task.done:
	default:
		t.Fatal("close did not join idle work")
	}
	if a.socketCount != 0 || len(a.paths) != 0 {
		t.Fatal("closed resources retained capacity")
	}
}

func TestAcquireReplacesClosingPathOnlyAfterSocketCloses(t *testing.T) {
	closeGate := make(chan struct{})
	first := newIdleTestSocket(false)
	first.closeGate = closeGate
	second := newIdleTestSocket(false)
	var unblocked sync.Once
	unblock := func() { unblocked.Do(func() { close(closeGate) }) }
	defer unblock()
	calls := 0
	a := New(Config{Capacity: 1, ListenPacket: func(string, string) (net.PacketConn, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}})
	t.Cleanup(func() { _ = a.Close() })
	addresses := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	lease, err := a.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	closing := runHandoff(lease.Close)
	receiveHandoff(t, first.closed)
	replacement := runHandoff(func() *Lease {
		next, acquireErr := a.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
		if acquireErr != nil {
			return nil
		}
		return next
	})
	if err := receiveHandoff(t, runHandoff(func() error {
		_, acquireErr := a.Acquire([16]byte{1}, 1, [16]byte{3}, addresses)
		return acquireErr
	})); err != ErrCapacity {
		t.Fatal(err)
	}
	unblock()
	if err := receiveHandoff(t, closing); err != nil {
		t.Fatal(err)
	}
	next := receiveHandoff(t, replacement)
	if next == nil || next.entry == lease.entry || calls != 2 {
		t.Fatal("acquisition reused a closing socket")
	}
}

func TestIdleReplacementRechecksRetirementAndDeadline(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "retirement"}[retire], func(t *testing.T) {
			socket := newIdleTestSocket(true)
			a, lease, servers := idleFixture(t, socket)
			var resume sync.Once
			unblock := func() { resume.Do(func() { close(socket.resume) }) }
			defer unblock()
			ctx := t.Context()
			// The already-expired parent proves deadline propagation without a sleep.
			if !retire {
				expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				if err := lease.StartIdle(expired, servers, time.Now().Add(idleTestBudget)); err != context.DeadlineExceeded {
					t.Fatal(err)
				}
				return
			}
			result := runHandoff(func() error { return lease.StartIdle(ctx, servers, time.Now().Add(idleTestBudget)) })
			receiveHandoff(t, socket.aborted)
			receiveHandoff(t, runHandoff(func() struct{} { a.Retire(1); return struct{}{} }))
			unblock()
			if err := receiveHandoff(t, result); err != ErrRetired {
				t.Fatal(err)
			}
		})
	}
}
