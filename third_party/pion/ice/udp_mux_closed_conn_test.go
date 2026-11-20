// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package ice

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
)

// recordingLogger captures mux errors, records the closed-connection trace drop
// for closedFrom, and signals when the probe drop from dropFrom has been logged.
// It implements every logging.LeveledLogger method, so no call can panic on a
// nil embedded logger.
type recordingLogger struct {
	mu            sync.Mutex
	errors        []string
	closedFrom    string
	closedDropped bool
	dropped       chan struct{}
	dropOnce      sync.Once
	dropFrom      string
}

func (l *recordingLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *recordingLogger) Tracef(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == "Dropping packet from "+l.closedFrom+" for closed connection" {
		l.mu.Lock()
		l.closedDropped = true
		l.mu.Unlock()
	}
	if msg == "Dropping packet from "+l.dropFrom {
		l.dropOnce.Do(func() { close(l.dropped) })
	}
}

func (l *recordingLogger) Trace(string)          {}
func (l *recordingLogger) Debug(string)          {}
func (l *recordingLogger) Debugf(string, ...any) {}
func (l *recordingLogger) Info(string)           {}
func (l *recordingLogger) Infof(string, ...any)  {}
func (l *recordingLogger) Warn(string)           {}
func (l *recordingLogger) Warnf(string, ...any)  {}
func (l *recordingLogger) Error(msg string)      { l.Errorf("%s", msg) }

var _ logging.LeveledLogger = (*recordingLogger)(nil)

// A packet from an address that still maps to a connection which is closed but
// not yet unregistered is routine at shutdown: the peer keeps sending until the
// removal catches up. It must not be reported as an error.
func TestUDPMuxDropsPacketsForClosedConnWithoutError(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	newPeer := func() *net.UDPConn {
		peer, dialErr := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		t.Cleanup(func() { _ = peer.Close() })

		return peer
	}
	closingPeer, probePeer := newPeer(), newPeer()
	closingAddr := closingPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	log := &recordingLogger{
		closedFrom: closingAddr.String(),
		dropped:    make(chan struct{}),
		dropFrom:   probePeer.LocalAddr().(*net.UDPAddr).AddrPort().String(),
	}
	mux := NewUDPMuxDefault(UDPMuxParams{Logger: log, UDPConn: listener})
	defer func() { _ = mux.Close() }()

	shared, err := mux.GetConn("closing", listener.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shared.Close() }()
	mux.mu.Lock()
	conn := mux.connsIPv4["closing"]
	mux.mu.Unlock()
	conn.addAddress(closingAddr)

	// Holding the registry lock keeps the removal that Close triggers pending,
	// which is the window a fast sender hits.
	func() {
		mux.mu.Lock()
		defer mux.mu.Unlock()

		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close muxed conn: %v", closeErr)
		}
		if _, writeErr := closingPeer.Write([]byte("late media")); writeErr != nil {
			t.Fatalf("write late packet: %v", writeErr)
		}
		// The worker reads datagrams in order, so the probe being dropped proves
		// the late packet has already been handled.
		if _, writeErr := probePeer.Write([]byte("probe")); writeErr != nil {
			t.Fatalf("write probe packet: %v", writeErr)
		}
		select {
		case <-log.dropped:
		case <-time.After(5 * time.Second):
			t.Fatal("mux did not process the probe packet")
		}
	}()

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.errors) != 0 {
		t.Fatalf("closing a connection reported errors for routine late packets: %q", log.errors)
	}
	if !log.closedDropped {
		t.Fatalf("expected trace drop for closed connection from %s", closingAddr)
	}
}
