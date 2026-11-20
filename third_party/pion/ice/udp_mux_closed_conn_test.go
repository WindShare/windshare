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

// recordingLogger captures mux errors and signals when the trace message for a
// dropped packet from dropFrom has been logged. It implements every
// logging.LeveledLogger method, so no call can panic on a nil embedded logger.
type recordingLogger struct {
	mu       sync.Mutex
	errors   []string
	dropped  chan struct{}
	dropOnce sync.Once
	dropFrom string
}

func (l *recordingLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *recordingLogger) Tracef(format string, args ...any) {
	if fmt.Sprintf(format, args...) == "Dropping packet from "+l.dropFrom {
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
	log := &recordingLogger{
		dropped:  make(chan struct{}),
		dropFrom: probePeer.LocalAddr().(*net.UDPAddr).AddrPort().String(),
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
	conn.addAddress(closingPeer.LocalAddr().(*net.UDPAddr).AddrPort())

	// Holding the registry lock keeps the removal that Close triggers pending,
	// which is the window a fast sender hits.
	mux.mu.Lock()
	_ = conn.Close()
	_, _ = closingPeer.Write([]byte("late media"))
	// The worker reads datagrams in order, so the probe being dropped proves the
	// late packet has already been handled.
	_, _ = probePeer.Write([]byte("probe"))
	select {
	case <-log.dropped:
	case <-time.After(5 * time.Second):
		t.Error("mux did not process the probe packet")
	}
	mux.mu.Unlock()

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.errors) != 0 {
		t.Fatalf("closing a connection reported errors for routine late packets: %q", log.errors)
	}
}
