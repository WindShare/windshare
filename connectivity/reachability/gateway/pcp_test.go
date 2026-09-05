package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

func request() r.Request {
	return r.Request{Endpoint: r.Endpoint{Generation: 1, Egress: "7", Local: netip.MustParseAddrPort("192.168.1.2:4000"), Protocol: r.UDP}, Lifetime: 120 * time.Second}
}
func pcpResponse(body []byte) []byte {
	response := make([]byte, 60)
	copy(response, body[:60])
	response[1] = 129
	binary.BigEndian.PutUint32(response[8:12], 100)
	binary.BigEndian.PutUint16(response[42:44], 55000)
	ip := netip.MustParseAddr("8.8.8.8").As16()
	copy(response[44:60], ip[:])
	return response
}
func TestPCPProxyActualTupleRenewRevoke(t *testing.T) {
	ctx := context.Background()
	req := request()
	var requests [][]byte
	p := &PCP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351"), Exchange: func(_ context.Context, local netip.Addr, server netip.AddrPort, body []byte) ([]byte, error) {
		if local != req.Endpoint.Local.Addr() || server.Port() != 5351 {
			t.Fatal("egress")
		}
		requests = append(requests, append([]byte(nil), body...))
		response := pcpResponse(body)
		if binary.BigEndian.Uint32(body[4:8]) != 0 {
			binary.BigEndian.PutUint32(response[4:8], 60)
		}
		return response, nil
	}}
	lease, err := p.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.External.Port() != 55000 || lease.Lifetime != time.Minute || lease.Kind != "ipv4-mapping" {
		t.Fatal(lease)
	}
	renewed, err := p.Renew(ctx, req, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Delete(ctx, req, renewed); err != nil {
		t.Fatal(err)
	}
	if string(requests[0][24:36]) != string(requests[1][24:36]) || binary.BigEndian.Uint16(requests[1][42:44]) != 55000 || binary.BigEndian.Uint32(requests[2][4:8]) != 0 {
		t.Fatal("renewal authority not retained")
	}
}
func TestPCPIPv6FilterAndFailures(t *testing.T) {
	req := request()
	req.Endpoint.Local = netip.MustParseAddrPort("[2606:4700::1]:4000")
	req.Scope.Remote = netip.MustParseAddrPort("[2001:4860::1]:5000")
	p := &PCP{Egress: "7", Server: netip.MustParseAddrPort("[fe80::1%7]:5351"), Nonce: func(b []byte) (int, error) {
		for i := range b {
			b[i] = 1
		}
		return len(b), nil
	}}
	p.Exchange = func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
		if len(body) != 84 || body[60] != 3 || body[65] != 128 || binary.BigEndian.Uint16(body[66:68]) != 5000 {
			t.Fatal("filter scope")
		}
		response := pcpResponse(body)
		ip := req.Endpoint.Local.Addr().As16()
		copy(response[44:60], ip[:])
		return response, nil
	}
	lease, err := p.Create(context.Background(), req)
	if err != nil || lease.Kind != "ipv6-pinhole" {
		t.Fatal(lease, err)
	}
	tests := []struct {
		name  string
		alter func([]byte) []byte
	}{
		{"short", func(b []byte) []byte { return b[:20] }},
		{"version", func(b []byte) []byte { b[0] = 1; return b }},
		{"opcode", func(b []byte) []byte { b[1] = 128; return b }},
		{"result", func(b []byte) []byte { b[3] = 2; return b }},
		{"nonce", func(b []byte) []byte { b[24]++; return b }},
		{"protocol", func(b []byte) []byte { b[36] = 6; return b }},
		{"internal", func(b []byte) []byte { b[40]++; return b }},
		{"private-proxy", func(b []byte) []byte { ip := netip.MustParseAddr("10.0.0.1").As16(); copy(b[44:60], ip[:]); return b }},
		{"long-ttl", func(b []byte) []byte { binary.BigEndian.PutUint32(b[4:8], 121); return b }},
		{"zero-port", func(b []byte) []byte { binary.BigEndian.PutUint16(b[42:44], 0); return b }},
		{"short-map", func(b []byte) []byte { return b[:24] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p.Exchange = func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
				return test.alter(pcpResponse(body)), nil
			}
			if _, err := p.Create(context.Background(), req); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
	p.Exchange = func(context.Context, netip.Addr, netip.AddrPort, []byte) ([]byte, error) {
		return nil, errors.New("network")
	}
	if _, err = p.Create(context.Background(), req); err == nil {
		t.Fatal("network")
	}
	p.Nonce = func([]byte) (int, error) { return 0, errors.New("entropy") }
	if _, err = p.Create(context.Background(), req); err == nil {
		t.Fatal("nonce")
	}
	if _, err = p.Renew(context.Background(), req, r.Lease{}); !errors.Is(err, r.ErrInvalid) {
		t.Fatal(err)
	}
	p.Egress = "wrong"
	if _, err = p.Renew(context.Background(), req, lease); !errors.Is(err, r.ErrUnavailable) {
		t.Fatal(err)
	}
}
