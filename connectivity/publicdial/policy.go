// Package publicdial pins untrusted discovery traffic to locally permitted public addresses.
package publicdial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/reachability"
)

var ErrAddress = errors.New("discovery requires a public HTTPS endpoint")

type Policy struct {
	Lookup func(context.Context, string, string) ([]netip.Addr, error)
	Permit func(netip.Addr) bool
	Dial   func(context.Context, string, string) (net.Conn, error)
}

func (p Policy) Permits(ip netip.Addr) bool {
	if p.Permit != nil {
		return p.Permit(ip.Unmap())
	}
	return reachability.PublicAddress(ip)
}

func (p Policy) Origin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrAddress
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || !p.Permits(ip) {
			return nil, ErrAddress
		}
	} else if !strings.Contains(strings.TrimSuffix(host, "."), ".") {
		return nil, ErrAddress
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}

func (p Policy) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	ips := []netip.Addr{ip}
	if err != nil {
		lookup := p.Lookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupNetIP
		}
		ips, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 4 * time.Second}).DialContext
	}
	last := ErrAddress
	for _, ip := range ips {
		ip = ip.Unmap()
		if !p.Permits(ip) || ip.Zone() != "" {
			continue
		}
		// The socket receives a numeric address while net/http retains the DNS name for TLS.
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func (p Policy) Client() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: p.dial, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 4 * time.Second, MaxResponseHeaderBytes: 16 * 1024}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
