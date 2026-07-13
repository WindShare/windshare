// wsrelay 是中转/信令服务器入口(执行计划 §6.8):WS hub 承载 register/join/
// 信令转发/回退数据面,HTTP 仅 health/config。二进制名取 wsrelay,避免与
// transport/relay 及过泛的 `relay` 混淆(§6.2)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

const (
	// defaultListenAddr 空 host = 所有网卡;端口无既定习惯,取一个不与
	// 常见开发服务(8080/3000/5173)相撞的值。
	defaultListenAddr = ":8484"
	// shutdownTimeout 是优雅退出时等 HTTP 面排空的上限;WS 连接不在其列
	// (被劫持的连接由 hub.Close 直接终结)。
	shutdownTimeout              = 5 * time.Second
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPReadTimeout       = 15 * time.Second
	defaultHTTPIdleTimeout       = 60 * time.Second
	defaultMaxHeaderBytes        = 1 << 20
	// JSON numbers above 2^53-1 are not exact in browser clients. Every integer
	// advertised by /v1/config must remain within that interoperable range.
	maxPublishedInteger = int64(1<<53 - 1)
)

type serverPolicy struct {
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	idleTimeout       time.Duration
	maxHeaderBytes    int
}

type connectionTracker struct {
	hub *signaling.Hub

	mu     sync.Mutex
	leases map[net.Conn]*signaling.ConnectionLease
}

func newConnectionTracker(hub *signaling.Hub) *connectionTracker {
	return &connectionTracker{hub: hub, leases: make(map[net.Conn]*signaling.ConnectionLease)}
}

func (t *connectionTracker) connContext(ctx context.Context, conn net.Conn) context.Context {
	lease, decision := t.hub.AdmitConnection(connectionSource(conn.RemoteAddr()))
	if !decision.Allowed() {
		_ = conn.Close()
		return ctx
	}
	t.mu.Lock()
	t.leases[conn] = lease
	t.mu.Unlock()
	return httpapi.WithConnectionLease(ctx, lease)
}

func (t *connectionTracker) connState(conn net.Conn, state http.ConnState) {
	if state != http.StateClosed && state != http.StateHijacked {
		return
	}
	t.mu.Lock()
	lease := t.leases[conn]
	delete(t.leases, conn)
	t.mu.Unlock()
	// A hijacked request transfers ownership to the handler/ServeConn path.
	if state == http.StateHijacked {
		lease.TransferToHandler()
	} else {
		lease.Release()
	}
}

func connectionSource(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

func (p serverPolicy) validate() error {
	switch {
	case p.readHeaderTimeout < time.Millisecond:
		return errors.New("wsrelay: http-read-header-timeout must be at least 1ms")
	case p.readTimeout < time.Millisecond:
		return errors.New("wsrelay: http-read-timeout must be at least 1ms")
	case p.readTimeout < p.readHeaderTimeout:
		return errors.New("wsrelay: http-read-timeout must not be shorter than http-read-header-timeout")
	case p.idleTimeout < time.Millisecond:
		return errors.New("wsrelay: http-idle-timeout must be at least 1ms")
	case p.maxHeaderBytes <= 0:
		return errors.New("wsrelay: http-max-header-bytes must be positive")
	case int64(p.maxHeaderBytes) > maxPublishedInteger:
		return errors.New("wsrelay: http-max-header-bytes exceeds the exact JSON integer range")
	}
	return nil
}

func validatePublishedAdmission(cfg admission.Config) error {
	limits := []struct {
		name  string
		value int64
	}{
		{"max-connections", int64(cfg.MaxConnections)},
		{"max-concurrent-shares", int64(cfg.MaxConcurrentShares)},
		{"max-shares-per-ip", int64(cfg.MaxSharesPerSource)},
		{"max-manifest-bytes-per-ip", cfg.MaxManifestBytesPerSource},
		{"max-total-manifest-bytes", cfg.MaxTotalManifestBytes},
		{"register-ip-burst", int64(cfg.RegisterPerSource.Burst)},
		{"join-ip-burst", int64(cfg.JoinPerSource.Burst)},
		{"join-share-burst", int64(cfg.JoinPerShare.Burst)},
	}
	for _, limit := range limits {
		if limit.value > maxPublishedInteger {
			return fmt.Errorf("wsrelay: %s exceeds the exact JSON integer range", limit.name)
		}
	}
	return nil
}

func validateWirePolicy(maxManifest, maxFrame int64, grace, roleTimeout, keepaliveTimeout time.Duration) error {
	switch {
	case maxManifest <= 0:
		return errors.New("wsrelay: max-manifest-bytes must be positive")
	case maxManifest > signaling.DefaultMaxManifestSize:
		return errors.New("wsrelay: max-manifest-bytes exceeds the protocol manifest limit")
	case maxFrame <= 0:
		return errors.New("wsrelay: max-frame-bytes must be positive")
	case maxFrame > session.MaxFrameSize:
		return errors.New("wsrelay: max-frame-bytes exceeds the protocol frame limit")
	case grace < time.Millisecond:
		return errors.New("wsrelay: sender-grace must be at least 1ms")
	case roleTimeout < time.Millisecond:
		return errors.New("wsrelay: ws-role-timeout must be at least 1ms")
	case keepaliveTimeout < time.Millisecond:
		return errors.New("wsrelay: ws-keepalive-timeout must be at least 1ms")
	}
	return nil
}

func (p serverPolicy) newServer(handler http.Handler, tracker *connectionTracker) *http.Server {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: p.readHeaderTimeout,
		ReadTimeout:       p.readTimeout,
		IdleTimeout:       p.idleTimeout,
		MaxHeaderBytes:    p.maxHeaderBytes,
	}
	if tracker != nil {
		server.ConnContext = tracker.connContext
		server.ConnState = tracker.connState
	}
	return server
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], nil, log.Printf)
	// Not a defer: log.Fatal exits the process and would skip it.
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

// run 与 main 分离以便测试驱动:onReady 在开始服务时携实际监听地址回调
// (支持 :0 随机端口),ctx 取消触发优雅退出。
func run(ctx context.Context, args []string, onReady func(net.Addr), logf func(string, ...any)) error {
	fs := flag.NewFlagSet("wsrelay", flag.ContinueOnError)
	admissionDefaults := admission.DefaultConfig()
	var (
		listen         = fs.String("listen", defaultListenAddr, "listen address in host:port form")
		origins        = fs.String("origins", "", "allowed WebSocket origins, comma-separated full origins such as https://windshare.top")
		allowLocalhost = fs.Bool("allow-localhost", true, "allow localhost/127.0.0.1 origins for development; disable after configuring -origins in production")

		maxManifest      = fs.Int64("max-manifest-bytes", signaling.DefaultMaxManifestSize, "maximum bytes per manifest")
		maxTotalManifest = fs.Int64("max-total-manifest-bytes", admissionDefaults.MaxTotalManifestBytes, "node-wide resident manifest memory budget")
		maxFrame         = fs.Int64("max-frame-bytes", session.MaxFrameSize, "maximum data-plane frame size")
		grace            = fs.Duration("sender-grace", protocol.SenderReconnectGrace, "sender re-registration grace period")

		maxConnections      = fs.Int("max-connections", admissionDefaults.MaxConnections, "maximum upgraded WebSocket connections")
		maxConcurrentShares = fs.Int("max-concurrent-shares", admissionDefaults.MaxConcurrentShares, "maximum live shares on this node")
		maxSharesPerIP      = fs.Int("max-shares-per-ip", admissionDefaults.MaxSharesPerSource, "maximum live shares charged to one source IP")
		maxManifestPerIP    = fs.Int64("max-manifest-bytes-per-ip", admissionDefaults.MaxManifestBytesPerSource, "maximum live manifest bytes charged to one source IP")
		registerIPRate      = fs.Float64("register-ip-rate", admissionDefaults.RegisterPerSource.PerSecond, "register attempts per second for one source IP")
		registerIPBurst     = fs.Int("register-ip-burst", admissionDefaults.RegisterPerSource.Burst, "register burst for one source IP")
		joinShareRate       = fs.Float64("join-share-rate", admissionDefaults.JoinPerShare.PerSecond, "join attempts per second for one share")
		joinShareBurst      = fs.Int("join-share-burst", admissionDefaults.JoinPerShare.Burst, "join burst for one share")
		joinIPRate          = fs.Float64("join-ip-rate", admissionDefaults.JoinPerSource.PerSecond, "join attempts per second for one source IP")
		joinIPBurst         = fs.Int("join-ip-burst", admissionDefaults.JoinPerSource.Burst, "join burst for one source IP")

		roleTimeout       = fs.Duration("ws-role-timeout", signaling.DefaultRoleTimeout, "maximum time to establish a WebSocket role or upload its manifest")
		keepaliveTimeout  = fs.Duration("ws-keepalive-timeout", signaling.DefaultKeepaliveTimeout, "maximum inactivity after WebSocket role establishment")
		readHeaderTimeout = fs.Duration("http-read-header-timeout", defaultHTTPReadHeaderTimeout, "maximum time to read HTTP request headers")
		readTimeout       = fs.Duration("http-read-timeout", defaultHTTPReadTimeout, "maximum time to read an HTTP request")
		idleTimeout       = fs.Duration("http-idle-timeout", defaultHTTPIdleTimeout, "maximum HTTP keep-alive idle time")
		maxHeaderBytes    = fs.Int("http-max-header-bytes", defaultMaxHeaderBytes, "maximum bytes in HTTP request headers")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	policy := serverPolicy{
		readHeaderTimeout: *readHeaderTimeout,
		readTimeout:       *readTimeout,
		idleTimeout:       *idleTimeout,
		maxHeaderBytes:    *maxHeaderBytes,
	}
	if err := policy.validate(); err != nil {
		return err
	}
	if err := validateWirePolicy(*maxManifest, *maxFrame, *grace, *roleTimeout, *keepaliveTimeout); err != nil {
		return err
	}
	admissionConfig := admission.Config{
		MaxConnections:            *maxConnections,
		MaxConcurrentShares:       *maxConcurrentShares,
		MaxSharesPerSource:        *maxSharesPerIP,
		MaxManifestBytesPerSource: *maxManifestPerIP,
		MaxTotalManifestBytes:     *maxTotalManifest,
		RegisterPerSource:         admission.Rate{PerSecond: *registerIPRate, Burst: *registerIPBurst},
		JoinPerSource:             admission.Rate{PerSecond: *joinIPRate, Burst: *joinIPBurst},
		JoinPerShare:              admission.Rate{PerSecond: *joinShareRate, Burst: *joinShareBurst},
	}
	controller, err := admission.NewController(admissionConfig)
	if err != nil {
		return fmt.Errorf("wsrelay: invalid admission policy: %w", err)
	}
	if err := validatePublishedAdmission(admissionConfig); err != nil {
		return err
	}
	if *maxManifest > admissionConfig.MaxManifestBytesPerSource || *maxManifest > admissionConfig.MaxTotalManifestBytes {
		return errors.New("wsrelay: max-manifest-bytes exceeds an enforced manifest budget")
	}

	hub := signaling.NewHub(signaling.Config{
		MaxManifestSize:      *maxManifest,
		MaxFrameSize:         *maxFrame,
		SenderReconnectGrace: *grace,
		RoleTimeout:          *roleTimeout,
		KeepaliveTimeout:     *keepaliveTimeout,
		Admission:            controller,
		Logf:                 logf,
	})
	defer hub.Close()

	var originList []string
	if *origins != "" {
		originList = strings.Split(*origins, ",")
	}
	publicLimits := hub.ServerLimits()
	publicLimits.MaxHeaderBytes = *maxHeaderBytes
	publicLimits.Timeouts.HTTPReadHeaderMilliseconds = policy.readHeaderTimeout.Milliseconds()
	publicLimits.Timeouts.HTTPReadMilliseconds = policy.readTimeout.Milliseconds()
	publicLimits.Timeouts.HTTPIdleMilliseconds = policy.idleTimeout.Milliseconds()
	apiConfig := httpapi.Config{
		Hub:            hub,
		AllowedOrigins: originList,
		AllowLocalhost: *allowLocalhost,
		Limits:         publicLimits,
	}
	if err := httpapi.ValidateConfig(apiConfig); err != nil {
		return fmt.Errorf("wsrelay: invalid HTTP policy: %w", err)
	}
	handler := httpapi.NewHandler(apiConfig)

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("wsrelay: listen on %s: %w", *listen, err)
	}
	tracker := newConnectionTracker(hub)
	srv := policy.newServer(handler, tracker)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	logf("wsrelay: listening on %s (protocol %s)", lis.Addr(), protocol.ProtocolVersion)
	if onReady != nil {
		onReady(lis.Addr())
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	// 优雅退出:先终结 WS 连接(劫持后的连接 Shutdown 管不到),再排空
	// 常规 HTTP 面。对端(发送端凭 resumeToken、接收端凭 rejoin)自带
	// 恢复语义,强断是安全的。
	hub.Close()
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return err
	}
	logf("wsrelay: stopped")
	return nil
}
