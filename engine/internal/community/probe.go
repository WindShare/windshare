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
	base, err := p.policy.Origin(d.RelayBaseURL())
	if err != nil {
		return err
	}
	var ip netip.Addr
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("/.well-known/windshare").String(), nil)
	if err != nil {
		return err
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if addr, ok := info.Conn.RemoteAddr().(*net.TCPAddr); ok {
			ip = addr.AddrPort().Addr().Unmap()
		}
	}}))
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	raw, err := readBounded(resp.Body, serviceauthority.MaximumDescriptorBytes)
	if err != nil {
		return err
	}
	key, err := serviceauthority.PublicKey(d.NodeKey)
	if err != nil {
		return err
	}
	live, err := serviceauthority.VerifyDescriptor(raw, key, p.now())
	if err != nil {
		return err
	}
	if live.Authority != nil || live.PublicRelay == serviceauthority.RelayDisabled || live.RelayEndpoint != d.RelayEndpoint || !p.policy.Permits(ip) {
		return serviceauthority.ErrFormat
	}
	if d.Services.Relay && d.PublicRelay == serviceauthority.RelayAnonymous {
		if !live.Services.Relay || live.PublicRelay != serviceauthority.RelayAnonymous {
			return errors.New("anonymous relay unavailable")
		}
		if err := p.verifyRelayOrigin(ctx, live.RelayBaseURL(), scope.Origin); err != nil {
			return err
		}
	}
	reach := observation{scope: scope, relay: d.PublicRelay == serviceauthority.RelayAnonymous && d.Services.Relay,
		until: time.Unix(int64(live.ExpiresAt), 0), ip: ip, stun: p.stunAddresses(live, ip, base.Hostname())}
	return p.remember(d, reach)
}

func (p *Pool) verifyRelayOrigin(ctx context.Context, endpoint, origin string) error {
	var id v2.ShareID
	rand.Read(id[:])
	ep, err := v2.RelayEndpointForShare(endpoint, id)
	if err != nil {
		return err
	}
	ws, _, err := websocket.Dial(ctx, ep.DialURL, &websocket.DialOptions{HTTPClient: p.http, HTTPHeader: http.Header{"Origin": {origin}}}) //nolint:bodyclose
	if err != nil {
		return err
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

func (p *Pool) remember(d serviceauthority.GossipDescriptor, reach observation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	latest, ok := p.peers[d.RelayEndpoint]
	if !ok || latest.d.NodeKey != d.NodeKey || latest.d.PublicRelay != d.PublicRelay || latest.d.Services != d.Services {
		return serviceauthority.ErrExpired
	}
	prefix := publicPrefix(reach.ip)
	sameIP, samePrefix := 0, 0
	for identity, other := range p.peers {
		if identity == d.RelayEndpoint || !other.live.until.After(p.now()) {
			continue
		}
		if other.live.ip == reach.ip {
			sameIP++
		}
		if prefix.Contains(other.live.ip) {
			samePrefix++
		}
	}
	if sameIP >= maxPeersPerIP || samePrefix >= maxPeersPerPrefix {
		return errors.New("verified community address quota")
	}
	latest.live = reach
	p.peers[d.RelayEndpoint] = latest
	return nil
}
