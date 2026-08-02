package relayv2_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/testloopback"
	"github.com/windshare/windshare/relay/connectionlimit"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

const (
	relayMaximumRoutes           = 4
	relayMaximumSessions         = 8
	relayMaximumSessionsPerShare = 4
	relayChallengeCapacity       = 8
	relayMaximumConnections      = 4

	relayHTTPReadHeaderTimeout = 2 * time.Second
	relayEndpointWriteTimeout  = 2 * time.Second
	relayCleanupTimeout        = 5 * time.Second
	relayDrainPollInterval     = 5 * time.Millisecond
	relayTombstoneFilename     = "stopped-shares.bin"
)

type relayRuntime struct {
	baseURL   string
	listener  *testloopback.TCPListener
	endpoint  *v2endpoint.Server
	server    *http.Server
	limiter   *connectionlimit.Limiter
	serveDone <-chan error

	closeOnce sync.Once
	closeErr  error
}

func startRelayRuntime(
	ctx context.Context,
	listener *testloopback.TCPListener,
	stateDirectory string,
) (*relayRuntime, error) {
	if listener == nil || stateDirectory == "" {
		return nil, errors.New("relay integration runtime configuration is invalid")
	}
	baseURL := (&url.URL{Scheme: "ws", Host: listener.Addr().String()}).String()
	relayEndpoint, err := v2.NormalizeRelayEndpoint(baseURL)
	if err != nil {
		return nil, fmt.Errorf("normalize relay integration endpoint: %w", err)
	}
	tombstones, err := v2route.NewFileTombstoneStore(filepath.Join(stateDirectory, relayTombstoneFilename))
	if err != nil {
		return nil, fmt.Errorf("create relay integration tombstone store: %w", err)
	}
	registry, err := v2route.New(ctx, v2route.Config{
		MaxRoutes:           relayMaximumRoutes,
		MaxSessions:         relayMaximumSessions,
		MaxSessionsPerShare: relayMaximumSessionsPerShare,
		Random:              rand.Reader,
		Tombstones:          tombstones,
	})
	if err != nil {
		return nil, fmt.Errorf("create relay integration registry: %w", err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: relayChallengeCapacity,
		Random:   rand.Reader,
	})
	if err != nil {
		return nil, fmt.Errorf("create relay integration challenge ledger: %w", err)
	}
	endpoint, err := v2endpoint.New(v2endpoint.Config{
		Registry:      registry,
		Challenges:    challenges,
		RelayIdentity: relayEndpoint.Identity,
		WriteTimeout:  relayEndpointWriteTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create relay integration endpoint: %w", err)
	}
	limiter, err := connectionlimit.New(connectionlimit.Config{
		MaximumConnections:          relayMaximumConnections,
		MaximumConnectionsPerSource: relayMaximumConnections,
	})
	if err != nil {
		return nil, fmt.Errorf("create relay integration connection limiter: %w", err)
	}
	handler := httpapi.NewV2Handler(httpapi.V2Config{
		Server:          endpoint,
		AllowLocalhost:  true,
		AdmitConnection: limiter.Admit,
	})
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: relayHTTPReadHeaderTimeout,
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()
	return &relayRuntime{
		baseURL: baseURL, listener: listener, endpoint: endpoint,
		server: httpServer, limiter: limiter, serveDone: serveResult,
	}, nil
}

func (runtime *relayRuntime) BaseURL() string {
	if runtime == nil {
		return ""
	}
	return runtime.baseURL
}

func (runtime *relayRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		runtime.closeErr = runtime.close()
	})
	return runtime.closeErr
}

func (runtime *relayRuntime) close() error {
	var failures []error
	if err := shutdownRelayEndpoint(runtime.endpoint); err != nil {
		failures = append(failures, err)
	}
	if err := shutdownRelayHTTPServer(runtime.server); err != nil {
		failures = append(failures, err)
		// A failed graceful shutdown remains a failed verdict, but force-closing
		// the listener prevents that diagnostic failure from leaking the owner.
		if closeErr := runtime.server.Close(); closeErr != nil &&
			!errors.Is(closeErr, http.ErrServerClosed) && !errors.Is(closeErr, net.ErrClosed) {
			failures = append(failures, fmt.Errorf("force-close relay HTTP server: %w", closeErr))
		}
	}
	if err := joinRelayServe(runtime.serveDone); err != nil {
		failures = append(failures, err)
	}
	if err := requireListenerClosed(runtime.listener); err != nil {
		failures = append(failures, err)
	}
	if err := requireConnectionsReleased(runtime.limiter); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func shutdownRelayEndpoint(endpoint *v2endpoint.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayCleanupTimeout)
	defer cancel()
	if err := endpoint.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown relay endpoint: %w", err)
	}
	return nil
}

func shutdownRelayHTTPServer(server *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayCleanupTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown relay HTTP server: %w", err)
	}
	return nil
}

func joinRelayServe(done <-chan error) error {
	timer := time.NewTimer(relayCleanupTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("relay HTTP serve loop: %w", err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("relay HTTP serve loop did not stop within %s", relayCleanupTimeout)
	}
}

func requireListenerClosed(listener *testloopback.TCPListener) error {
	timer := time.NewTimer(relayCleanupTimeout)
	defer timer.Stop()
	select {
	case <-listener.Closed():
		return nil
	case <-timer.C:
		return fmt.Errorf("owned relay listener did not close within %s", relayCleanupTimeout)
	}
}

func requireConnectionsReleased(limiter *connectionlimit.Limiter) error {
	timer := time.NewTimer(relayCleanupTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(relayDrainPollInterval)
	defer ticker.Stop()
	for {
		snapshot := limiter.Snapshot()
		if snapshot.Connections == 0 && snapshot.Sources == 0 {
			return nil
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return fmt.Errorf(
				"relay connection owner did not drain within %s: connections=%d sources=%d",
				relayCleanupTimeout,
				snapshot.Connections,
				snapshot.Sources,
			)
		}
	}
}
