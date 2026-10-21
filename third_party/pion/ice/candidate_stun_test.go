// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package ice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/stun/v4"
)

func TestRelayPermissionWaitDoesNotBlockICEControl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent, err := NewAgent(&AgentConfig{MulticastDNSMode: MulticastDNSModeDisabled})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = agent.Close() }()
		slow, slowConn := stunTestCandidate(t, agent, 4001)
		fast, fastConn := stunTestCandidate(t, agent, 4002)
		defer func() { _ = slow.close(); _ = fast.close() }()
		close(fastConn.release)
		remote := stunTestRemote(t)
		message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		returned := make(chan struct{})
		go func() {
			_ = agent.loop.Run(agent.loop, func(context.Context) { agent.sendSTUN(message, slow, remote) })
			close(returned)
		}()
		<-slowConn.started
		progress := make(chan struct{})
		go func() {
			_ = agent.loop.Run(agent.loop, func(context.Context) {
				agent.sendSTUN(message, fast, remote)
				close(progress)
			})
		}()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Error("TURN permission round trip blocked the ICE event loop")
		}
		select {
		case <-progress:
		default:
			t.Error("one TURN allocation prevented another candidate from progressing")
		}
		if len(fastConn.written) != 1 {
			t.Error("healthy candidate could not send while another permission was pending")
		}
		close(slowConn.release)
		synctest.Wait()
	})
}

func TestRelaySTUNQueueOwnsPacketsAndBoundsPendingWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agent, err := NewAgent(&AgentConfig{MulticastDNSMode: MulticastDNSModeDisabled})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = agent.Close() }()
		local, conn := stunTestCandidate(t, agent, 4001)
		defer func() { _ = local.close() }()
		remote := stunTestRemote(t)
		raw := []byte("original packet")
		if err := local.writeSTUN(raw, remote); err != nil {
			t.Fatal(err)
		}
		<-conn.started
		for range maxQueuedSTUNPackets {
			if err := local.writeSTUN(raw, remote); err != nil {
				t.Fatal(err)
			}
		}
		if err := local.writeSTUN(raw, remote); !errors.Is(err, errSTUNQueueFull) {
			t.Fatalf("full queue: %v", err)
		}
		if err := local.writeSTUN(make([]byte, receiveMTU+1), remote); !errors.Is(err, errSTUNPacketTooLarge) {
			t.Fatalf("oversized packet: %v", err)
		}
		clear(raw)
		close(conn.release)
		synctest.Wait()
		if len(conn.written) != maxQueuedSTUNPackets+1 {
			t.Fatalf("accepted packets were lost: %d", len(conn.written))
		}
		for len(conn.written) > 0 {
			if got := <-conn.written; !bytes.Equal(got, []byte("original packet")) {
				t.Fatalf("queued write retained borrowed payload: %q", got)
			}
		}
		if err := local.close(); err != nil {
			t.Fatal(err)
		}
		if err := local.writeSTUN(raw, remote); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed candidate accepted a write: %v", err)
		}
	})
}

func TestRelaySTUNQueueExpiresChecksAndJoinsOnClose(t *testing.T) {
	for _, closePending := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "closed"}[closePending], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				agent, err := NewAgent(&AgentConfig{MulticastDNSMode: MulticastDNSModeDisabled})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = agent.Close() }()
				local, conn := stunTestCandidate(t, agent, 4001)
				defer func() { _ = local.close() }()
				remote := stunTestRemote(t)
				if err := local.writeSTUN([]byte("in flight"), remote); err != nil {
					t.Fatal(err)
				}
				<-conn.started
				if err := local.writeSTUN([]byte("queued"), remote); err != nil {
					t.Fatal(err)
				}
				if closePending {
					if err := local.close(); err != nil {
						t.Fatal(err)
					}
					select {
					case <-local.stunWriter.done:
					default:
						t.Fatal("candidate close returned with a live writer")
					}
					if len(conn.written) != 0 {
						t.Fatal("closed candidate sent queued checks")
					}
				} else {
					time.Sleep(maxBindingRequestTimeout)
					close(conn.release)
					synctest.Wait()
					if len(conn.written) != 1 || string(<-conn.written) != "in flight" {
						t.Fatal("obsolete checks were sent after permission wait")
					}
				}
			})
		})
	}
}

type stunTestPacketConn struct {
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	written chan []byte
	once    sync.Once
}

func (conn *stunTestPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-conn.closed
	return 0, nil, net.ErrClosed
}

func (conn *stunTestPacketConn) WriteTo(raw []byte, _ net.Addr) (int, error) {
	select {
	case conn.started <- struct{}{}:
	default:
	}
	select {
	case <-conn.closed:
		return 0, net.ErrClosed
	case <-conn.release:
		conn.written <- append([]byte(nil), raw...)
		return len(raw), nil
	}
}

func (conn *stunTestPacketConn) Close() error {
	conn.once.Do(func() { close(conn.closed) })
	return nil
}

func (*stunTestPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*stunTestPacketConn) SetDeadline(time.Time) error      { return nil }
func (*stunTestPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (*stunTestPacketConn) SetWriteDeadline(time.Time) error { return nil }

func stunTestCandidate(t *testing.T, agent *Agent, port int) (*CandidateRelay, *stunTestPacketConn) {
	t.Helper()
	candidate, err := NewCandidateRelay(&CandidateRelayConfig{
		Network: "udp", Address: "192.0.2.1", Port: port, Component: ComponentRTP,
		RelAddr: "192.0.2.2", RelPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn := &stunTestPacketConn{
		started: make(chan struct{}, 128), release: make(chan struct{}), closed: make(chan struct{}),
		written: make(chan []byte, 128),
	}
	initialized := make(chan struct{})
	close(initialized)
	candidate.start(agent, conn, initialized)
	return candidate, conn
}

func stunTestRemote(t *testing.T) Candidate {
	t.Helper()
	candidate, err := NewCandidateHost(&CandidateHostConfig{
		Network: "udp", Address: "198.51.100.1", Port: 5000, Component: ComponentRTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}
