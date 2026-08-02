package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/internal/testloopback"
)

type relayCutProxy struct {
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc

	mu          sync.Mutex
	target      string
	targetReady chan struct{}
	connections map[net.Conn]struct{}
	active      int
	changed     chan struct{}
	stopping    bool
	firstErr    error

	acceptDone chan struct{}
	downstream atomic.Uint64
	cutOnce    sync.Once
	waitOnce   sync.Once
	waitBytes  uint64
	waitErr    error
}

func startRelayCutProxy(t *testing.T, scenario *v2Scenario) *relayCutProxy {
	t.Helper()
	loopback := testloopback.New(t)
	scenario.trace.RequireCleanup(t, "relay cut proxy loopback sockets", func(context.Context) error {
		return loopback.Close()
	})
	listener := loopback.ListenTCP()
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &relayCutProxy{
		listener:    listener,
		ctx:         ctx,
		cancel:      cancel,
		targetReady: make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
		changed:     make(chan struct{}),
		acceptDone:  make(chan struct{}),
	}
	go proxy.accept()
	cleanup := func(ctx context.Context) error {
		_, err := proxy.CutAndWait(ctx)
		return err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
		defer cancel()
		if err := cleanup(ctx); err != nil {
			t.Errorf("stop relay cut proxy: %v", err)
		}
	})
	scenario.trace.RequireCleanup(t, "relay cut proxy", cleanup)
	return proxy
}

func (proxy *relayCutProxy) BaseURL() string {
	return "ws://" + proxy.listener.Addr().String()
}

func (proxy *relayCutProxy) ForwardTo(address string) error {
	if _, err := net.ResolveTCPAddr("tcp", address); err != nil {
		return fmt.Errorf("resolve relay proxy target: %w", err)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping {
		return errors.New("relay cut proxy is stopping")
	}
	if proxy.target != "" {
		return errors.New("relay cut proxy target is already bound")
	}
	proxy.target = address
	close(proxy.targetReady)
	return nil
}

func (proxy *relayCutProxy) CutAndWait(ctx context.Context) (uint64, error) {
	proxy.cutOnce.Do(func() {
		proxy.mu.Lock()
		proxy.stopping = true
		connections := make([]net.Conn, 0, len(proxy.connections))
		for connection := range proxy.connections {
			connections = append(connections, connection)
		}
		proxy.mu.Unlock()

		proxy.cancel()
		_ = proxy.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
	proxy.waitOnce.Do(func() {
		proxy.waitErr = waitRelayProxyQuiescence(ctx, proxy.acceptDone, proxy.activitySnapshot)
		proxy.waitBytes = proxy.downstream.Load()
	})
	return proxy.waitBytes, proxy.waitErr
}

func (proxy *relayCutProxy) accept() {
	defer close(proxy.acceptDone)
	for {
		connection, err := proxy.listener.Accept()
		if err != nil {
			if !proxy.isStopping() {
				proxy.recordError(fmt.Errorf("accept relay proxy connection: %w", err))
			}
			return
		}
		if !proxy.retainFront(connection) {
			_ = connection.Close()
			return
		}
		go proxy.serve(connection)
	}
}

func (proxy *relayCutProxy) serve(front net.Conn) {
	defer proxy.releaseFront(front)

	target, ok := proxy.awaitTarget()
	if !ok {
		return
	}
	backend, err := (&net.Dialer{}).DialContext(proxy.ctx, "tcp", target)
	if err != nil {
		if !proxy.isStopping() {
			proxy.recordError(fmt.Errorf("connect relay proxy backend: %w", err))
		}
		return
	}
	if !proxy.retainBackend(backend) {
		_ = backend.Close()
		return
	}
	defer proxy.releaseConnection(backend)

	results := make(chan error, 2)
	proxy.addActivities(2)
	go func() {
		defer proxy.finishActivity()
		_, copyErr := io.Copy(backend, front)
		results <- copyErr
	}()
	go func() {
		defer proxy.finishActivity()
		_, copyErr := io.Copy(relayDownstreamWriter{target: front, bytes: &proxy.downstream}, backend)
		results <- copyErr
	}()
	select {
	case <-results:
	case <-proxy.ctx.Done():
	}
	// A WebSocket is one full-duplex authority. Once either half ends, retaining
	// the other half could let a cut race leave a hidden relay path alive.
	_ = front.Close()
	_ = backend.Close()
	select {
	case <-results:
	case <-proxy.ctx.Done():
	}
}

func (proxy *relayCutProxy) awaitTarget() (string, bool) {
	select {
	case <-proxy.ctx.Done():
		return "", false
	case <-proxy.targetReady:
		proxy.mu.Lock()
		target := proxy.target
		proxy.mu.Unlock()
		return target, target != ""
	}
}

func (proxy *relayCutProxy) retainFront(connection net.Conn) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping {
		return false
	}
	proxy.connections[connection] = struct{}{}
	proxy.active++
	proxy.notifyLocked()
	return true
}

func (proxy *relayCutProxy) retainBackend(connection net.Conn) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping {
		return false
	}
	proxy.connections[connection] = struct{}{}
	return true
}

func (proxy *relayCutProxy) releaseFront(connection net.Conn) {
	proxy.mu.Lock()
	delete(proxy.connections, connection)
	proxy.active--
	proxy.notifyLocked()
	proxy.mu.Unlock()
	_ = connection.Close()
}

func (proxy *relayCutProxy) releaseConnection(connection net.Conn) {
	proxy.mu.Lock()
	delete(proxy.connections, connection)
	proxy.mu.Unlock()
	_ = connection.Close()
}

func (proxy *relayCutProxy) addActivities(count int) {
	proxy.mu.Lock()
	proxy.active += count
	proxy.notifyLocked()
	proxy.mu.Unlock()
}

func (proxy *relayCutProxy) finishActivity() {
	proxy.mu.Lock()
	proxy.active--
	proxy.notifyLocked()
	proxy.mu.Unlock()
}

func (proxy *relayCutProxy) activitySnapshot() (int, <-chan struct{}, error) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.active, proxy.changed, proxy.firstErr
}

func (proxy *relayCutProxy) notifyLocked() {
	close(proxy.changed)
	proxy.changed = make(chan struct{})
}

func (proxy *relayCutProxy) isStopping() bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.stopping
}

func (proxy *relayCutProxy) recordError(err error) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if !proxy.stopping && proxy.firstErr == nil {
		proxy.firstErr = err
	}
}

type relayDownstreamWriter struct {
	target io.Writer
	bytes  *atomic.Uint64
}

func (writer relayDownstreamWriter) Write(value []byte) (int, error) {
	written, err := writer.target.Write(value)
	writer.bytes.Add(uint64(written))
	return written, err
}
