package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

// Config 配置 HTTP/策略面。限额通告若留零值,取与 hub 一致的默认。
type Config struct {
	Hub *signaling.Hub
	// AllowedOrigins:允许发起 WS 的浏览器 Origin 白名单(完整 origin,如
	// https://windshare.top),由部署方配置(§6.8);自部署中转可放行自部署
	// 前端。大小写不敏感。
	AllowedOrigins []string
	// AllowLocalhost:dev 放行任意 localhost/127.0.0.1/[::1] 来源(§6.8)。
	AllowLocalhost bool
	// Limits 经 /v1/config 通告,供客户端预检(§6.7)。须与 hub 的执行值
	// 一致——通告与执行不一致比不通告更糟。
	Limits protocol.ServerLimits
	// SourceIdentity derives the admission identity from the request. Production
	// defaults to the TCP peer and deliberately ignores spoofable forwarding
	// headers; trusted-proxy deployments can inject their own verified policy.
	SourceIdentity func(*http.Request) string
}

type connectionLeaseContextKey struct{}

// WithConnectionLease transfers a listener-admitted TCP lease to the request
// handler. The production server uses it to cover slow headers and keep-alives;
// standalone handlers fall back to admission immediately before upgrade.
func WithConnectionLease(ctx context.Context, lease *signaling.ConnectionLease) context.Context {
	return context.WithValue(ctx, connectionLeaseContextKey{}, lease)
}

func connectionLeaseFromContext(ctx context.Context) (*signaling.ConnectionLease, bool) {
	lease, ok := ctx.Value(connectionLeaseContextKey{}).(*signaling.ConnectionLease)
	return lease, ok && lease != nil
}

func (c Config) withDefaults() Config {
	if c.Hub != nil {
		enforced := c.Hub.ServerLimits()
		fillServerLimitDefaults(&c.Limits, enforced)
	} else {
		if c.Limits.MaxFrameSize <= 0 {
			c.Limits.MaxFrameSize = session.MaxFrameSize
		}
		if c.Limits.MaxManifestSize <= 0 {
			c.Limits.MaxManifestSize = signaling.DefaultMaxManifestSize
		}
		if c.Limits.MaxSignalingMessageBytes <= 0 {
			c.Limits.MaxSignalingMessageBytes = protocol.MaxSignalingMessageBytes
		}
	}
	if c.SourceIdentity == nil {
		c.SourceIdentity = remoteIP
	}
	return c
}

func fillServerLimitDefaults(target *protocol.ServerLimits, enforced protocol.ServerLimits) {
	if target.MaxFrameSize == 0 {
		target.MaxFrameSize = enforced.MaxFrameSize
	}
	if target.MaxManifestSize == 0 {
		target.MaxManifestSize = enforced.MaxManifestSize
	}
	if target.MaxSignalingMessageBytes == 0 {
		target.MaxSignalingMessageBytes = enforced.MaxSignalingMessageBytes
	}
	if target.Admission == (protocol.AdmissionLimits{}) {
		target.Admission = enforced.Admission
	}
	if target.Timeouts.WebSocketRoleMilliseconds == 0 {
		target.Timeouts.WebSocketRoleMilliseconds = enforced.Timeouts.WebSocketRoleMilliseconds
	}
	if target.Timeouts.WebSocketKeepaliveMilliseconds == 0 {
		target.Timeouts.WebSocketKeepaliveMilliseconds = enforced.Timeouts.WebSocketKeepaliveMilliseconds
	}
	if target.Timeouts.SenderReconnectGraceMilliseconds == 0 {
		target.Timeouts.SenderReconnectGraceMilliseconds = enforced.Timeouts.SenderReconnectGraceMilliseconds
	}
}

// ValidateConfig fails malformed deployment policy before the listener starts.
// An invalid allowlist must never degrade into a running deny-all server.
func ValidateConfig(cfg Config) error {
	if cfg.Hub == nil {
		return errors.New("httpapi: Hub is required")
	}
	cfg = cfg.withDefaults()
	advertised := cfg.Limits
	advertised.MaxHeaderBytes = 0
	advertised.Timeouts.HTTPReadHeaderMilliseconds = 0
	advertised.Timeouts.HTTPReadMilliseconds = 0
	advertised.Timeouts.HTTPIdleMilliseconds = 0
	if enforced := cfg.Hub.ServerLimits(); advertised != enforced {
		return errors.New("httpapi: advertised relay limits differ from enforced Hub policy")
	}
	_, err := normalizeOrigins(cfg.AllowedOrigins)
	return err
}

// NewHandler 组装中转的全部 HTTP 面:health/config 与 WS 升级入口。
// 不含静态托管——前端由站点/CDN 分离部署(§6.8),同域需求交给反向代理。
func NewHandler(cfg Config) http.Handler {
	cfg = cfg.withDefaults()
	if err := ValidateConfig(cfg); err != nil {
		panic(err)
	}
	allowed, _ := normalizeOrigins(cfg.AllowedOrigins)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.Health{Status: protocol.HealthOK})
	})

	// 版本列表让客户端在建 WS 前发现错配并明确报错(§6.7);当前仅 v1。
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, protocol.ServerConfig{
			ProtocolVersions: []string{protocol.ProtocolVersion},
			Limits:           cfg.Limits,
		})
	})

	mux.HandleFunc("GET /v1/ws/{shareId}", func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !originAllowed(origins[0], allowed, cfg.AllowLocalhost)) {
			http.Error(w, "origin is not allowed", http.StatusForbidden)
			return
		}
		shareID := r.PathValue("shareId")
		if err := protocol.ValidateShareID(shareID); err != nil {
			http.Error(w, "invalid shareId", http.StatusNotFound)
			return
		}
		source := cfg.SourceIdentity(r)
		connectionLease, preAdmitted := connectionLeaseFromContext(r.Context())
		if !preAdmitted {
			var decision admission.Decision
			connectionLease, decision = cfg.Hub.AdmitConnection(source)
			if !decision.Allowed() {
				http.Error(w, "relay connection capacity exceeded", http.StatusServiceUnavailable)
				return
			}
			defer connectionLease.Release()
		} else {
			defer connectionLease.ReleaseIfTransferred()
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Origin 已按白名单(含 localhost dev 例外)校验;库内建的
			// 同源检查只会误拦白名单来源,故关闭。
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		cfg.Hub.ServeConn(r.Context(), ws, shareID, source, connectionLease)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// normalizeOrigins validates and canonicalizes the deployment allowlist before
// serving. Silently dropping a typo would leave an apparently healthy relay
// that denies every intended browser at runtime.
func normalizeOrigins(origins []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		n, ok := normalizeOrigin(o)
		if !ok {
			return nil, fmt.Errorf("httpapi: invalid allowed origin %q", o)
		}
		set[n] = struct{}{}
	}
	return set, nil
}

func normalizeOrigin(origin string) (string, bool) {
	origin = strings.TrimSpace(origin)
	// Query and fragment delimiters are invalid even when their value is empty;
	// net/url otherwise loses that distinction for a trailing '#'.
	if origin == "" || strings.ContainsAny(origin, "?#") {
		return "", false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.ForceQuery ||
		u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", false
	}
	if ip := net.ParseIP(hostname); ip != nil {
		hostname = ip.String()
	} else if strings.Contains(hostname, ":") || isNumericHost(hostname) {
		return "", false
	}
	port := u.Port()
	if port == "" && strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		port = strconv.FormatUint(n, 10)
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, true
}

func isNumericHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// originAllowed 执行 WS Origin 白名单(§6.8)。无 Origin 头 = 非浏览器
// 客户端(CLI/原生),放行——Origin 检查防的是"浏览器被第三方页面驱动
// 连中转"这类跨站滥用,非浏览器客户端伪造 Origin 本就无从防起。
func originAllowed(origin string, allowed map[string]struct{}, allowLocalhost bool) bool {
	if origin == "" {
		return true
	}
	n, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	if _, ok := allowed[n]; ok {
		return true
	}
	if allowLocalhost {
		u, _ := url.Parse(n)
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

// remoteIP 提取对端 IP 供 admission。取 TCP 对端而非 X-Forwarded-For:
// 后者可伪造,信任它须与"部署在可信反代之后"的配置绑定,留待部署面需要
// 时再引入。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
