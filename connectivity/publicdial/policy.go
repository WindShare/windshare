// Package publicdial pins untrusted discovery traffic to locally permitted public addresses.
package publicdial

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/urlhost"
)

const (
	defaultDialTimeout         = 4 * time.Second
	defaultFallbackDelay       = 250 * time.Millisecond
	defaultIdleConnTimeout     = 30 * time.Second
	defaultTLSHandshakeTimeout = 4 * time.Second
	maxIdleConns               = 4
	maxResponseHeaderBytes     = 16 * 1024
	maxCandidatesPerFamily     = 4
	maxDialCandidates          = 6
	httpsDefaultPort           = 443
)

var ErrAddress = errors.New("discovery requires a public HTTPS endpoint")

type Policy struct {
	Lookup          func(context.Context, string, string) ([]netip.Addr, error)
	Permit          func(netip.Addr) bool
	Dial            func(context.Context, string, string) (net.Conn, error)
	FallbackDelay   time.Duration
	TLSClientConfig *tls.Config
}

func (p Policy) Permits(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if p.Permit != nil {
		return p.Permit(ip)
	}
	return reachability.PublicAddress(ip)
}

// Origin validates a public HTTPS origin (root path "" or "/" only), suitable
// for configured discovery seeds where non-root paths indicate user typos.
func (p Policy) Origin(raw string) (*url.URL, error) {
	return p.parseOrigin(raw, false)
}

// RelayOrigin extracts the root discovery origin from a public HTTPS relay base
// URL, allowing valid subpath mounts because /.well-known/windshare and
// /discovery/v1/peers always live at the origin root.
func (p Policy) RelayOrigin(raw string) (*url.URL, error) {
	return p.parseOrigin(raw, true)
}

func (p Policy) parseOrigin(raw string, allowSubpath bool) (*url.URL, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsAny(raw, "#\\") ||
		strings.IndexFunc(raw, forbiddenOriginRune) >= 0 {
		return nil, ErrAddress
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || strings.HasSuffix(u.Host, ":") {
		return nil, ErrAddress
	}
	if err := validateOriginPath(u, allowSubpath); err != nil {
		return nil, err
	}
	if err := urlhost.Normalize(u); err != nil {
		return nil, ErrAddress
	}
	port, err := normalizeHTTPSPort(u.Port())
	if err != nil {
		return nil, err
	}
	host, err := p.normalizeOriginHost(u.Hostname(), strings.HasPrefix(u.Host, "["))
	if err != nil {
		return nil, err
	}
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	u.Path, u.RawPath = "", ""
	return u, nil
}

func validateOriginPath(u *url.URL, allowSubpath bool) error {
	if allowSubpath {
		if !validSubpath(u.EscapedPath()) {
			return ErrAddress
		}
		return nil
	}
	if (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return ErrAddress
	}
	return nil
}

func normalizeHTTPSPort(port string) (string, error) {
	if port == "" {
		return "", nil
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return "", ErrAddress
	}
	if value == httpsDefaultPort {
		return "", nil
	}
	return strconv.FormatUint(value, 10), nil
}

func (p Policy) normalizeOriginHost(hostname string, bracketed bool) (string, error) {
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if bracketed != ip.Is6() || !p.Permits(ip) {
			return "", ErrAddress
		}
		return ip.Unmap().String(), nil
	}
	if bracketed {
		return "", ErrAddress
	}
	host := strings.ToLower(hostname)
	if err := networkendpoint.ValidateDNSHost(host); err != nil {
		return "", ErrAddress
	}
	if !strings.Contains(strings.TrimSuffix(host, "."), ".") || isReservedDNSHost(host) {
		return "", ErrAddress
	}
	return host, nil
}

func forbiddenOriginRune(r rune) bool {
	return r <= 0x1f || r == 0x7f || unicode.IsSpace(r) || r == '\ufeff'
}

func isReservedDNSHost(host string) bool {
	trimmed := strings.TrimSuffix(host, ".")
	suffix := trimmed
	if _, after, ok := strings.CutLast(trimmed, "."); ok {
		suffix = after
	}
	switch suffix {
	case "localhost", "local", "internal", "invalid", "onion", "arpa":
		return true
	default:
		return false
	}
}

func validSubpath(escapedPath string) bool {
	for segment := range strings.SplitSeq(escapedPath, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !utf8.ValidString(decoded) || decoded == "." || decoded == ".." {
			return false
		}
	}
	return true
}

func (p Policy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	candidates, err := p.dialCandidates(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrAddress
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: defaultDialTimeout}).DialContext
	}
	if len(candidates) == 1 {
		// The socket receives a numeric address while net/http retains the DNS name for TLS.
		return dial(ctx, network, net.JoinHostPort(candidates[0].String(), port))
	}
	delay := p.FallbackDelay
	if delay <= 0 {
		delay = defaultFallbackDelay
	}
	return dialStaggered(ctx, network, port, candidates, dial, delay)
}

func (p Policy) dialCandidates(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !p.Permits(ip) {
			return nil, ErrAddress
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return selectDialCandidates(ips, p.Permits), nil
}

// selectDialCandidates filters permitted addresses, bounds per-family fanout,
// and interleaves the preferred and fallback address families so a long AAAA
// list cannot starve IPv4 fallback.
func selectDialCandidates(ips []netip.Addr, permits func(netip.Addr) bool) []netip.Addr {
	var primaries, fallbacks []netip.Addr
	seen := make(map[netip.Addr]struct{}, min(len(ips), maxDialCandidates))
	var primaryIs6 bool
	for _, raw := range ips {
		if !permits(raw) {
			continue
		}
		ip := raw.Unmap()
		if _, dup := seen[ip]; dup {
			continue
		}
		seen[ip] = struct{}{}
		if len(primaries) == 0 {
			primaryIs6 = ip.Is6()
		}
		if ip.Is6() == primaryIs6 {
			if len(primaries) < maxCandidatesPerFamily {
				primaries = append(primaries, ip)
			}
		} else if len(fallbacks) < maxCandidatesPerFamily {
			fallbacks = append(fallbacks, ip)
		}
	}
	out := make([]netip.Addr, 0, min(len(primaries)+len(fallbacks), maxDialCandidates))
	for i := 0; len(out) < maxDialCandidates && (i < len(primaries) || i < len(fallbacks)); i++ {
		if i < len(primaries) {
			out = append(out, primaries[i])
		}
		if i < len(fallbacks) && len(out) < maxDialCandidates {
			out = append(out, fallbacks[i])
		}
	}
	return out
}

// dialStaggered restores RFC 8305-style staggered dual-stack fallback that manual
// numeric-IP pinning bypasses in net.Dialer, preventing a blackholed primary IP
// from consuming the entire request deadline before trying the other family.
func dialStaggered(
	ctx context.Context,
	network, port string,
	candidates []netip.Addr,
	dial func(context.Context, string, string) (net.Conn, error),
	delay time.Duration,
) (net.Conn, error) {
	raceCtx, cancelRace := context.WithCancel(ctx)
	defer cancelRace()

	var (
		mu       sync.Mutex
		finished bool
		winner   net.Conn
	)
	winCh := make(chan struct{}, 1)
	errCh := make(chan error, len(candidates))

	start := func(ip netip.Addr) {
		addr := net.JoinHostPort(ip.String(), port)
		go func() {
			conn, err := dial(raceCtx, network, addr)
			if err != nil {
				errCh <- err
				return
			}
			mu.Lock()
			if finished {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			finished = true
			winner = conn
			mu.Unlock()
			cancelRace()
			winCh <- struct{}{}
		}()
	}

	next := 0
	inflight := 0
	launch := func() {
		start(candidates[next])
		next++
		inflight++
	}

	launch()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	timerCh := timer.C

	var lastErr error
	for inflight > 0 || next < len(candidates) {
		select {
		case <-winCh:
			return winner, nil
		case <-ctx.Done():
			mu.Lock()
			finished = true
			leaked := winner
			winner = nil
			mu.Unlock()
			if leaked != nil {
				_ = leaked.Close()
			}
			return nil, ctx.Err()
		case err := <-errCh:
			inflight--
			lastErr = err
			if inflight == 0 && next < len(candidates) {
				launch()
				if next < len(candidates) {
					timer.Reset(delay)
					timerCh = timer.C
				} else {
					timer.Stop()
					timerCh = nil
				}
			}
		case <-timerCh:
			launch()
			if next < len(candidates) {
				timer.Reset(delay)
			} else {
				timerCh = nil
			}
		}
	}
	if lastErr == nil {
		return nil, ErrAddress
	}
	return nil, lastErr
}

type httpsTransport struct {
	base *http.Transport
}

func (t httpsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !strings.EqualFold(req.URL.Scheme, "https") {
		return nil, ErrAddress
	}
	return t.base.RoundTrip(req)
}

func (t httpsTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

func (p Policy) Client() *http.Client {
	transport := &http.Transport{
		// Proxy stays nil so environment proxy variables cannot bypass public-IP pinning.
		DialContext:            p.dial,
		MaxIdleConns:           maxIdleConns,
		IdleConnTimeout:        defaultIdleConnTimeout,
		TLSHandshakeTimeout:    defaultTLSHandshakeTimeout,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	if p.TLSClientConfig != nil {
		transport.TLSClientConfig = p.TLSClientConfig.Clone()
	}
	return &http.Client{
		Transport: httpsTransport{base: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
