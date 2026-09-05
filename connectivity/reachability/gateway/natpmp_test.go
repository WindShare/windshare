package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"

	r "github.com/windshare/windshare/connectivity/reachability"
)

func pmpResponse(body []byte) []byte {
	if len(body) == 2 {
		response := []byte{0, 128, 0, 0, 0, 0, 0, 100, 8, 8, 8, 8}
		return response
	}
	response := make([]byte, 16)
	response[1] = body[1] + 128
	binary.BigEndian.PutUint32(response[4:8], 100)
	copy(response[8:10], body[4:6])
	binary.BigEndian.PutUint16(response[10:12], 55000)
	copy(response[12:16], body[8:12])
	return response
}
func TestNATPMPActualPortAndProtocol(t *testing.T) {
	for _, protocol := range []r.Protocol{r.UDP, r.TCP} {
		req := request()
		req.Endpoint.Protocol = protocol
		var bodies [][]byte
		p := &NATPMP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351"), Exchange: func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
			bodies = append(bodies, append([]byte(nil), body...))
			return pmpResponse(body), nil
		}}
		lease, err := p.Create(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if lease.External.Port() != 55000 || lease.External.Addr().String() != "8.8.8.8" {
			t.Fatal(lease)
		}
		lease, err = p.Renew(context.Background(), req, lease)
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(bodies[3][6:8]) != 55000 {
			t.Fatal("renew requested original port")
		}
		if err = p.Delete(context.Background(), req, lease); err != nil {
			t.Fatal(err)
		}
		if len(bodies) != 5 || binary.BigEndian.Uint32(bodies[4][8:12]) != 0 {
			t.Fatal("delete")
		}
	}
}
func TestNATPMPErrors(t *testing.T) {
	req := request()
	p := &NATPMP{Egress: "7", Server: netip.MustParseAddrPort("192.168.1.1:5351")}
	cases := []func([]byte) []byte{
		func(b []byte) []byte { return b[:1] },
		func(b []byte) []byte { b[0] = 1; return b },
		func(b []byte) []byte { b[1] = 0; return b },
		func(b []byte) []byte { b[3] = 2; return b },
		func(b []byte) []byte {
			if len(b) == 12 {
				copy(b[8:12], []byte{10, 0, 0, 1})
			}
			return b
		},
		func(b []byte) []byte {
			if len(b) == 16 {
				b[8]++
			}
			return b
		},
		func(b []byte) []byte {
			if len(b) == 16 {
				b[10] = 0
				b[11] = 0
			}
			return b
		},
		func(b []byte) []byte {
			if len(b) == 16 {
				binary.BigEndian.PutUint32(b[12:16], 121)
			}
			return b
		},
		func(b []byte) []byte {
			if len(b) == 16 {
				binary.BigEndian.PutUint32(b[4:8], 99)
			}
			return b
		},
	}
	for i, alter := range cases {
		p.Exchange = func(_ context.Context, _ netip.Addr, _ netip.AddrPort, body []byte) ([]byte, error) {
			return alter(pmpResponse(body)), nil
		}
		if _, err := p.Create(context.Background(), req); err == nil {
			t.Fatalf("case %d", i)
		}
	}
	p.Exchange = func(context.Context, netip.Addr, netip.AddrPort, []byte) ([]byte, error) {
		return nil, errors.New("network")
	}
	if _, err := p.Create(context.Background(), req); err == nil {
		t.Fatal("network")
	}
	req.Scope.Remote = netip.MustParseAddrPort("1.1.1.1:4000")
	if _, err := p.Create(context.Background(), req); !errors.Is(err, r.ErrUnavailable) {
		t.Fatal("restriction")
	}
}
