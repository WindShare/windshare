package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

type relayProxyActivitySnapshot func() (int, <-chan struct{}, error)

func waitRelayProxyActivities(ctx context.Context, snapshot relayProxyActivitySnapshot) error {
	if ctx == nil || snapshot == nil {
		return errors.New("relay proxy cleanup authority is incomplete")
	}
	for {
		active, changed, observedErr := snapshot()
		if active < 0 || changed == nil {
			return errors.Join(errors.New("relay proxy activity accounting is invalid"), observedErr)
		}
		if active == 0 {
			return observedErr
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("relay proxy activity did not quiesce: %w", ctx.Err()), observedErr)
		case <-changed:
		}
	}
}

func waitRelayProxyQuiescence(
	ctx context.Context,
	acceptDone <-chan struct{},
	snapshot relayProxyActivitySnapshot,
) error {
	if ctx == nil || acceptDone == nil || snapshot == nil {
		return errors.New("relay proxy cleanup authority is incomplete")
	}
	accepting := true
	for {
		active, changed, observedErr := snapshot()
		if active < 0 || changed == nil {
			return errors.Join(errors.New("relay proxy activity accounting is invalid"), observedErr)
		}
		if !accepting && active == 0 {
			return observedErr
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("relay proxy cleanup did not quiesce: %w", ctx.Err()), observedErr)
		case <-acceptDone:
			accepting = false
			acceptDone = nil
		case <-changed:
		}
	}
}

func TestRelayProxyCleanupDeadlineIsAFailedVerdict(t *testing.T) {
	for name, invoke := range map[string]func(context.Context) error{
		"cut accept syscall": func(ctx context.Context) error {
			lifecycle, cancel := context.WithCancel(context.Background())
			proxy := &relayCutProxy{
				listener: &stalledRelayListener{}, ctx: lifecycle, cancel: cancel,
				targetReady: make(chan struct{}), connections: make(map[net.Conn]struct{}),
				changed: make(chan struct{}), acceptDone: make(chan struct{}),
			}
			_, err := proxy.CutAndWait(ctx)
			return err
		},
		"cut handler": func(ctx context.Context) error {
			lifecycle, cancel := context.WithCancel(context.Background())
			acceptDone := make(chan struct{})
			close(acceptDone)
			proxy := &relayCutProxy{
				listener: &stalledRelayListener{}, ctx: lifecycle, cancel: cancel,
				targetReady: make(chan struct{}), connections: make(map[net.Conn]struct{}),
				active: 1, changed: make(chan struct{}), acceptDone: acceptDone,
			}
			_, err := proxy.CutAndWait(ctx)
			return err
		},
		"pause accept syscall": func(ctx context.Context) error {
			lifecycle, cancel := context.WithCancel(context.Background())
			connectionContext, cancelConnections := context.WithCancel(lifecycle)
			proxy := &relayPauseProxy{
				listener: &stalledRelayListener{}, ctx: lifecycle, cancel: cancel,
				connectionContext: connectionContext, cancelConnections: cancelConnections,
				connections: make(map[net.Conn]struct{}), changed: make(chan struct{}),
				acceptDone: make(chan struct{}),
			}
			return proxy.Close(ctx)
		},
		"pause handler": func(ctx context.Context) error {
			lifecycle, cancel := context.WithCancel(context.Background())
			connectionContext, cancelConnections := context.WithCancel(lifecycle)
			acceptDone := make(chan struct{})
			close(acceptDone)
			proxy := &relayPauseProxy{
				listener: &stalledRelayListener{}, ctx: lifecycle, cancel: cancel,
				connectionContext: connectionContext, cancelConnections: cancelConnections,
				connections: make(map[net.Conn]struct{}), active: 1, changed: make(chan struct{}),
				acceptDone: acceptDone,
			}
			return proxy.Close(ctx)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := invoke(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cleanup timeout was not a failed verdict: %v", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cleanup exceeded its caller-owned deadline envelope: %v", elapsed)
			}
		})
	}
}

type stalledRelayListener struct{}

func (*stalledRelayListener) Accept() (net.Conn, error) { return nil, errors.New("not started") }
func (*stalledRelayListener) Close() error              { return nil }
func (*stalledRelayListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
