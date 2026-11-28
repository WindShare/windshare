package community

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/serviceauthority"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	maxPeersPerIP     = 2
	maxPeersPerPrefix = 4
)

func (p *Pool) verify(ctx context.Context, scope Scope, entry peer) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	d := entry.d
	base, err := p.policy.RelayOrigin(d.RelayBaseURL())
	if err != nil {
		return &probeError{kind: probeFailureRejected, reason: "invalid_origin", cause: err}
	}
	var ip netip.Addr
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("/.well-known/windshare").String(), nil)
	if err != nil {
		return &probeError{kind: probeFailureRejected, reason: "invalid_request", cause: err}
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if addr, ok := info.Conn.RemoteAddr().(*net.TCPAddr); ok {
			ip = addr.AddrPort().Addr().Unmap()
		}
	}}))
	resp, err := p.http.Do(req)
	if err != nil {
		return &probeError{kind: probeFailureTransient, reason: "descriptor_fetch_failed", cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			return &probeError{kind: probeFailureTransient, reason: "descriptor_http_transient", cause: errors.New(resp.Status)}
		}
		return &probeError{kind: probeFailureRejected, reason: "descriptor_http_rejected", cause: errors.New(resp.Status)}
	}
	raw, err := readBounded(resp.Body, serviceauthority.MaximumDescriptorBytes)
	if err != nil {
		if errors.Is(err, serviceauthority.ErrFormat) {
			return &probeError{kind: probeFailureRejected, reason: "descriptor_oversized", cause: err}
		}
		return &probeError{kind: probeFailureTransient, reason: "descriptor_read_failed", cause: err}
	}
	key, err := serviceauthority.PublicKey(d.NodeKey)
	if err != nil {
		return &probeError{kind: probeFailureRejected, reason: "descriptor_key_invalid", cause: err}
	}
	live, err := serviceauthority.VerifyDescriptor(raw, key, p.now())
	if err != nil {
		return &probeError{kind: probeFailureRejected, reason: "descriptor_invalid", cause: err}
	}
	if live.RelayEndpoint != d.RelayEndpoint || !p.policy.Permits(ip) {
		return &probeError{kind: probeFailureRejected, reason: "descriptor_endpoint_or_policy", cause: serviceauthority.ErrFormat}
	}
	if live.Authority != nil || live.PublicRelay == serviceauthority.RelayDisabled {
		reason := "live_disabled"
		if live.Authority != nil {
			reason = "live_authority"
		}
		return &probeError{kind: probeFailureEvict, reason: reason, issuedAt: live.IssuedAt, cause: serviceauthority.ErrFormat}
	}
	until := time.Unix(int64(live.ExpiresAt), 0)
	host := hostObservation{
		generation: scope.Generation,
		until:      until,
		ip:         ip,
		stun:       p.stunAddresses(live, ip, base.Hostname()),
	}
	if d.Services.Relay && d.PublicRelay == serviceauthority.RelayAnonymous {
		if !live.Services.Relay || live.PublicRelay != serviceauthority.RelayAnonymous {
			// Admit origin-independent STUN from the freshly signed live descriptor
			// even while anonymous relaying is temporarily exhausted or unavailable.
			p.rememberHost(d, host, true)
			return &probeError{
				kind:         probeFailureRelayUnavailable,
				reason:       liveRelayReason(live),
				until:        until,
				issuedAt:     live.IssuedAt,
				hostVerified: true,
				cause:        errors.New("anonymous relay unavailable"),
			}
		}
		if err := p.verifyRelayOrigin(ctx, live.RelayBaseURL(), scope.Origin); err != nil {
			p.rememberHost(d, host, false)
			pe := classifyProbeError(err)
			pe.hostVerified = true
			return pe
		}
		return p.remember(d, scope, host, relayObservation{scope: scope, until: until})
	}
	return p.remember(d, scope, host, relayObservation{})
}

func (p *Pool) verifyRelayOrigin(ctx context.Context, endpoint, origin string) error {
	var id v2.ShareID
	rand.Read(id[:])
	ep, err := v2.RelayEndpointForShare(endpoint, id)
	if err != nil {
		return &probeError{kind: probeFailureRejected, reason: "invalid_relay_endpoint", cause: err}
	}
	ws, resp, err := websocket.Dial(ctx, ep.DialURL, &websocket.DialOptions{HTTPClient: p.http, HTTPHeader: http.Header{"Origin": {origin}}}) //nolint:bodyclose
	if err != nil {
		if resp != nil {
			switch {
			case resp.StatusCode == http.StatusForbidden:
				return &probeError{kind: probeFailureRejected, reason: "origin_forbidden", cause: err}
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
				return &probeError{kind: probeFailureTransient, reason: "relay_upgrade_transient", cause: err}
			case resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError:
				return &probeError{kind: probeFailureRejected, reason: "relay_upgrade_rejected", cause: err}
			}
		}
		return &probeError{kind: probeFailureTransient, reason: "relay_upgrade_failed", cause: err}
	}
	_ = ws.CloseNow()
	return nil
}

func (p *Pool) stunAddresses(live serviceauthority.Descriptor, ip netip.Addr, relayHost string) []string {
	var stun []string
	if live.Services.STUN {
		for _, address := range live.STUN {
			normalized, err := networkendpoint.NormalizeSTUN(address)
			if err != nil {
				continue
			}
			host, port, err := net.SplitHostPort(strings.TrimPrefix(normalized, "stun:"))
			if err != nil || !strings.EqualFold(host, relayHost) {
				p.trace("community_stun_rejected", address, "host_mismatch")
				continue
			}
			// Pion receives a checked numeric destination. No later DNS lookup
			// can change the address between ICE selection and the UDP write.
			stun = append(stun, "stun:"+net.JoinHostPort(ip.String(), port))
			p.trace("community_stun_admitted", address, ip.String())
		}
	}
	return stun
}

func (p *Pool) withinAddressQuotaLocked(endpoint string, ip netip.Addr) bool {
	prefix := publicPrefix(ip)
	sameIP, samePrefix := 0, 0
	for identity, other := range p.peers {
		if identity == endpoint || !other.host.until.After(p.now()) {
			continue
		}
		if other.host.ip == ip {
			sameIP++
		}
		if prefix.Contains(other.host.ip) {
			samePrefix++
		}
	}
	return sameIP < maxPeersPerIP && samePrefix < maxPeersPerPrefix
}

func (p *Pool) rememberHost(d serviceauthority.GossipDescriptor, host hostObservation, clearRelay bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	latest, ok := p.peers[d.RelayEndpoint]
	if !ok || latest.d.NodeKey != d.NodeKey || latest.d.PublicRelay != d.PublicRelay || latest.d.Services != d.Services {
		return
	}
	if len(host.stun) == 0 {
		latest.host.stun = nil
		if clearRelay || !latest.relay.until.After(p.now()) {
			latest.host = hostObservation{}
		}
		p.peers[d.RelayEndpoint] = latest
		return
	}
	if !p.withinAddressQuotaLocked(d.RelayEndpoint, host.ip) {
		return
	}
	latest.host = host
	p.peers[d.RelayEndpoint] = latest
}

func (p *Pool) remember(d serviceauthority.GossipDescriptor, scope Scope, host hostObservation, relay relayObservation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	latest, ok := p.peers[d.RelayEndpoint]
	if !ok || latest.d.NodeKey != d.NodeKey || latest.d.PublicRelay != d.PublicRelay || latest.d.Services != d.Services {
		return serviceauthority.ErrExpired
	}
	if !p.withinAddressQuotaLocked(d.RelayEndpoint, host.ip) {
		return &probeError{kind: probeFailureRejected, reason: "address_quota", cause: errors.New("verified community address quota")}
	}
	latest.host = host
	if relay.scope.Origin != "" {
		latest.relay = relay
	}
	latest.clearFailure(scope)
	p.peers[d.RelayEndpoint] = latest
	return nil
}
