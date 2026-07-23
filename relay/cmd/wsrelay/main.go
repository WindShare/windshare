package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/windshare/windshare/relay/connectionlimit"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

const (
	defaultListenAddress              = ":8484"
	defaultMaximumRoutes              = 1_024
	defaultMaximumSessions            = 4_096
	defaultMaximumSessionsPerShare    = 64
	defaultChallengeCapacity          = 4_096
	defaultHTTPReadHeaderTimeout      = 5 * time.Second
	defaultHTTPReadTimeout            = 15 * time.Second
	defaultHTTPIdleTimeout            = 60 * time.Second
	defaultMaximumHTTPHeaderBytes     = 1 << 20
	defaultEndpointWriteTimeout       = 15 * time.Second
	shutdownTimeout                   = 5 * time.Second
	tombstoneFilename                 = "stopped-shares.bin"
	defaultRelayStateDirectoryName    = "WindShare"
	defaultRelayStateSubdirectoryName = "relay"
)

type serverPolicy struct {
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	idleTimeout       time.Duration
	maximumHeader     int
}

func (policy serverPolicy) validate() error {
	switch {
	case policy.readHeaderTimeout < time.Millisecond:
		return errors.New("wsrelay: http-read-header-timeout must be at least 1ms")
	case policy.readTimeout < policy.readHeaderTimeout:
		return errors.New("wsrelay: http-read-timeout must not be shorter than http-read-header-timeout")
	case policy.idleTimeout < time.Millisecond:
		return errors.New("wsrelay: http-idle-timeout must be at least 1ms")
	case policy.maximumHeader <= 0:
		return errors.New("wsrelay: http-max-header-bytes must be positive")
	default:
		return nil
	}
}

func (policy serverPolicy) newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: policy.readHeaderTimeout, ReadTimeout: policy.readTimeout,
		IdleTimeout: policy.idleTimeout, MaxHeaderBytes: policy.maximumHeader,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], nil, log.Printf)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string, onReady func(net.Addr), logf func(string, ...any)) error {
	flags := flag.NewFlagSet("wsrelay", flag.ContinueOnError)
	var (
		listenAddress  = flags.String("listen", defaultListenAddress, "listen address in host:port form")
		relayBaseURL   = flags.String("relay-base-url", "", "public relay base URL; required when clients do not use the listener address")
		stateDirectory = flags.String("state-dir", "", "durable relay state directory")
		origins        = flags.String("origins", "", "allowed WebSocket origins, comma-separated full origins")
		allowLocalhost = flags.Bool("allow-localhost", true, "allow localhost origins for development")

		maximumConnections   = flags.Int("max-connections", connectionlimit.DefaultMaximumConnections, "maximum upgraded WebSocket connections")
		maximumPerSource     = flags.Int("max-connections-per-source", connectionlimit.DefaultMaximumConnectionsPerSource, "maximum upgraded WebSockets per source")
		maximumRoutes        = flags.Int("max-routes", defaultMaximumRoutes, "maximum live and permanently stopped routes")
		maximumSessions      = flags.Int("max-sessions", defaultMaximumSessions, "maximum active and recently ended relay sessions")
		maximumShareSessions = flags.Int("max-sessions-per-share", defaultMaximumSessionsPerShare, "maximum relay sessions for one share")
		challengeCapacity    = flags.Int("challenge-capacity", defaultChallengeCapacity, "maximum outstanding one-use authentication challenges")

		endpointWriteTimeout = flags.Duration("ws-write-timeout", defaultEndpointWriteTimeout, "maximum time to write one WebSocket frame")
		readHeaderTimeout    = flags.Duration("http-read-header-timeout", defaultHTTPReadHeaderTimeout, "maximum time to read HTTP headers")
		readTimeout          = flags.Duration("http-read-timeout", defaultHTTPReadTimeout, "maximum time to read an HTTP request")
		idleTimeout          = flags.Duration("http-idle-timeout", defaultHTTPIdleTimeout, "maximum HTTP keep-alive idle time")
		maximumHeader        = flags.Int("http-max-header-bytes", defaultMaximumHTTPHeaderBytes, "maximum bytes in HTTP request headers")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("wsrelay: unexpected positional arguments")
	}
	policy := serverPolicy{
		readHeaderTimeout: *readHeaderTimeout, readTimeout: *readTimeout,
		idleTimeout: *idleTimeout, maximumHeader: *maximumHeader,
	}
	if err := policy.validate(); err != nil {
		return err
	}
	if *endpointWriteTimeout <= 0 || *maximumRoutes <= 0 || *maximumSessions <= 0 ||
		*maximumShareSessions <= 0 || *maximumShareSessions > *maximumSessions || *challengeCapacity <= 0 {
		return errors.New("wsrelay: invalid v2 route, session, challenge, or write limit")
	}
	limiter, err := connectionlimit.New(connectionlimit.Config{
		MaximumConnections: *maximumConnections, MaximumConnectionsPerSource: *maximumPerSource,
	})
	if err != nil {
		return fmt.Errorf("wsrelay: connection admission: %w", err)
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("wsrelay: listen on %s: %w", *listenAddress, err)
	}
	listenerOwned := true
	defer func() {
		if listenerOwned {
			_ = listener.Close()
		}
	}()
	endpoint, err := publicRelayEndpoint(*relayBaseURL, listener.Addr())
	if err != nil {
		return err
	}
	relayStateDirectory, err := resolveStateDirectory(*stateDirectory, endpoint.Identity)
	if err != nil {
		return err
	}
	tombstones, err := v2route.NewFileTombstoneStore(filepath.Join(relayStateDirectory, tombstoneFilename))
	if err != nil {
		return fmt.Errorf("wsrelay: initialize STOP tombstones: %w", err)
	}
	registry, err := v2route.New(ctx, v2route.Config{
		MaxRoutes: *maximumRoutes, MaxSessions: *maximumSessions,
		MaxSessionsPerShare: *maximumShareSessions, Random: rand.Reader, Tombstones: tombstones,
	})
	if err != nil {
		return fmt.Errorf("wsrelay: initialize route registry: %w", err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: *challengeCapacity, Random: rand.Reader})
	if err != nil {
		return fmt.Errorf("wsrelay: initialize challenge ledger: %w", err)
	}
	endpointServer, err := v2endpoint.New(v2endpoint.Config{
		Registry: registry, Challenges: challenges, RelayIdentity: endpoint.Identity,
		WriteTimeout: *endpointWriteTimeout,
		RetirementTracer: v2endpoint.RetirementTraceFunc(func(event v2endpoint.RetirementTrace) {
			// A generation mismatch is expected during same-ID replacement races;
			// retaining both labels makes the safety decision reconstructable.
			logf(
				"wsrelay: route_retirement connection_id=%s local_generation=%d current_generation=%d source=%s target=%s compare_result=%s applied=%t",
				event.ConnectionID, event.LocalGeneration, event.CurrentGeneration,
				event.Source, event.Target, event.CompareResult, event.Applied,
			)
		}),
	})
	if err != nil {
		return fmt.Errorf("wsrelay: initialize v2 endpoint: %w", err)
	}

	var allowedOrigins []string
	if *origins != "" {
		for origin := range strings.SplitSeq(*origins, ",") {
			allowedOrigins = append(allowedOrigins, strings.TrimSpace(origin))
		}
	}
	httpConfig := httpapi.V2Config{
		Server: endpointServer, AllowedOrigins: allowedOrigins, AllowLocalhost: *allowLocalhost,
		AdmitConnection: limiter.Admit,
	}
	if err := httpapi.ValidateV2Config(httpConfig); err != nil {
		return fmt.Errorf("wsrelay: invalid HTTP policy: %w", err)
	}
	v2Handler := httpapi.NewV2Handler(httpConfig)
	mux := http.NewServeMux()
	mux.Handle("/v2/ws", v2Handler)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("ok\n"))
	})
	server := policy.newServer(mux)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	listenerOwned = false
	logf("wsrelay: listening on %s (protocol v2, identity %s)", listener.Addr(), endpoint.IdentityURL)
	if onReady != nil {
		onReady(listener.Addr())
	}

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	serverErr := server.Shutdown(shutdownContext)
	endpointErr := endpointServer.Shutdown(shutdownContext)
	if err := errors.Join(serverErr, endpointErr); err != nil {
		return fmt.Errorf("wsrelay: shutdown: %w", err)
	}
	logf("wsrelay: stopped")
	return nil
}

func publicRelayEndpoint(configured string, address net.Addr) (v2.RelayEndpoint, error) {
	base := configured
	if base == "" {
		host, port, err := net.SplitHostPort(address.String())
		if err != nil {
			return v2.RelayEndpoint{}, fmt.Errorf("wsrelay: derive relay identity: %w", err)
		}
		ip := net.ParseIP(host)
		if host == "" || ip != nil && ip.IsUnspecified() {
			host = "127.0.0.1"
		}
		base = (&url.URL{Scheme: "ws", Host: net.JoinHostPort(host, port)}).String()
	}
	endpoint, err := v2.NormalizeRelayEndpoint(base)
	if err != nil {
		return v2.RelayEndpoint{}, fmt.Errorf("wsrelay: invalid relay-base-url: %w", err)
	}
	return endpoint, nil
}

func resolveStateDirectory(configured string, identity v2.RelayIdentity) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("wsrelay: locate user configuration directory: %w", err)
	}
	identityName := hex.EncodeToString(identity[:8])
	return filepath.Join(root, defaultRelayStateDirectoryName, defaultRelayStateSubdirectoryName, identityName), nil
}
