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

	"github.com/windshare/windshare/internal/testnetwork"
)

type relayCutProxy struct {
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc

	mu          sync.Mutex
	target      string
	targetReady chan struct{}
	connections map[net.Conn]struct{}
	stopping    bool
	firstErr    error

	acceptDone chan struct{}
	handlers   sync.WaitGroup
	downstream atomic.Uint64
	cutOnce    sync.Once
	stopped    chan struct{}
}

func startRelayCutProxy(t *testing.T) *relayCutProxy {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start relay cut proxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &relayCutProxy{
		listener:    listener,
		ctx:         ctx,
		cancel:      cancel,
		targetReady: make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
		acceptDone:  make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	go proxy.accept()
	t.Cleanup(func() {
		if _, err := proxy.CutAndWait(); err != nil {
			t.Errorf("stop relay cut proxy: %v", err)
		}
	})
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

func (proxy *relayCutProxy) CutAndWait() (uint64, error) {
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
		<-proxy.acceptDone
		proxy.handlers.Wait()
		close(proxy.stopped)
	})
	<-proxy.stopped

	proxy.mu.Lock()
	err := proxy.firstErr
	proxy.mu.Unlock()
	return proxy.downstream.Load(), err
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
		if !proxy.retain(connection) {
			_ = connection.Close()
			return
		}
		proxy.handlers.Add(1)
		go proxy.serve(connection)
	}
}

func (proxy *relayCutProxy) serve(front net.Conn) {
	testnetwork.AssertOSNetwork()
	defer proxy.handlers.Done()
	defer proxy.release(front)

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
	if !proxy.retain(backend) {
		_ = backend.Close()
		return
	}
	defer proxy.release(backend)

	results := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(backend, front)
		results <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(relayDownstreamWriter{target: front, bytes: &proxy.downstream}, backend)
		results <- copyErr
	}()
	<-results
	// A WebSocket is one full-duplex authority. Once either half ends, retaining
	// the other half could let a cut race leave a hidden relay path alive.
	_ = front.Close()
	_ = backend.Close()
	<-results
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

func (proxy *relayCutProxy) retain(connection net.Conn) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping {
		return false
	}
	proxy.connections[connection] = struct{}{}
	return true
}

func (proxy *relayCutProxy) release(connection net.Conn) {
	proxy.mu.Lock()
	delete(proxy.connections, connection)
	proxy.mu.Unlock()
	_ = connection.Close()
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
