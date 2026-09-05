package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

const pcpMapLength = 60

type PCP struct {
	Server   netip.AddrPort
	Egress   string
	Exchange Exchange
	Nonce    func([]byte) (int, error)
}
type pcpToken struct {
	Nonce [12]byte
	Epoch uint32
}

func (p *PCP) Create(ctx context.Context, request r.Request) (r.Lease, error) {
	token := pcpToken{}
	nonce := p.Nonce
	if nonce == nil {
		nonce = rand.Read
	}
	if _, err := nonce(token.Nonce[:]); err != nil {
		return r.Lease{}, err
	}
	return p.mapping(ctx, request, r.Lease{Token: token}, false)
}
func (p *PCP) Renew(ctx context.Context, request r.Request, lease r.Lease) (r.Lease, error) {
	return p.mapping(ctx, request, lease, false)
}
func (p *PCP) Delete(ctx context.Context, request r.Request, lease r.Lease) error {
	request.Lifetime = 0
	_, err := p.mapping(ctx, request, lease, true)
	return err
}
func (p *PCP) mapping(ctx context.Context, request r.Request, previous r.Lease, deleting bool) (r.Lease, error) {
	if err := ctx.Err(); err != nil {
		return r.Lease{}, err
	}
	if request.Endpoint.Egress != p.Egress || !p.Server.IsValid() || request.Endpoint.Local.Addr().Is4() != p.Server.Addr().Is4() {
		return r.Lease{}, r.ErrUnavailable
	}
	token, ok := previous.Token.(pcpToken)
	if !ok {
		return r.Lease{}, r.ErrInvalid
	}
	body := make([]byte, pcpMapLength)
	body[0] = 2
	body[1] = 1
	binary.BigEndian.PutUint32(body[4:8], uint32(request.Lifetime/time.Second))
	local := request.Endpoint.Local.Addr().As16()
	copy(body[8:24], local[:])
	copy(body[24:36], token.Nonce[:])
	body[36] = byte(request.Endpoint.Protocol)
	binary.BigEndian.PutUint16(body[40:42], request.Endpoint.Local.Port())
	if previous.External.IsValid() {
		binary.BigEndian.PutUint16(body[42:44], previous.External.Port())
		external := previous.External.Addr().As16()
		copy(body[44:60], external[:])
	}
	if request.Scope.Remote.IsValid() {
		// FILTER is optional code 3 and precisely preserves the remote restriction.
		filter := make([]byte, 24)
		filter[0] = 3
		binary.BigEndian.PutUint16(filter[2:4], 20)
		filter[5] = 128
		binary.BigEndian.PutUint16(filter[6:8], request.Scope.Remote.Port())
		remote := request.Scope.Remote.Addr().As16()
		copy(filter[8:24], remote[:])
		body = append(body, filter...)
	}
	exchange := p.Exchange
	if exchange == nil {
		exchange = DatagramExchange
	}
	response, err := exchange(ctx, request.Endpoint.Local.Addr(), p.Server, body)
	if err != nil {
		return r.Lease{}, err
	}
	if len(response) < 24 || len(response) > maxDatagram || len(response)%4 != 0 || response[0] != 2 || response[1] != 129 {
		return r.Lease{}, r.ErrInvalidResponse
	}
	if response[3] != 0 {
		return r.Lease{}, fmt.Errorf("%w: PCP result %d", r.ErrUnavailable, response[3])
	}
	if len(response) < pcpMapLength || !bytes.Equal(response[24:36], token.Nonce[:]) || response[36] != body[36] || !bytes.Equal(response[40:42], body[40:42]) {
		return r.Lease{}, r.ErrInvalidResponse
	}
	lifetime := time.Duration(binary.BigEndian.Uint32(response[4:8])) * time.Second
	if deleting {
		if lifetime != 0 {
			return r.Lease{}, r.ErrInvalidResponse
		}
		return r.Lease{}, nil
	}
	var ip [16]byte
	copy(ip[:], response[44:60])
	external := netip.AddrPortFrom(netip.AddrFrom16(ip).Unmap(), binary.BigEndian.Uint16(response[42:44]))
	// A proxy's effective upstream lifetime and outermost tuple are authoritative;
	// private first-hop mappings never become public candidate facts.
	if lifetime <= 0 || lifetime > request.Lifetime || external.Port() == 0 || !r.PublicAddress(external.Addr()) {
		if lifetime > 0 {
			return r.Lease{}, revokeInvalid(ctx, p, request, r.Lease{External: external, Token: token})
		}
		return r.Lease{}, fmt.Errorf("%w: %w", r.ErrLeaseLost, r.ErrInvalidResponse)
	}
	epoch := binary.BigEndian.Uint32(response[8:12])
	// A successful MAP after restart has already recreated this resource. Keep
	// its new authority instead of rejecting every renewal until the old epoch.
	restarted := token.Epoch > 0 && epoch < token.Epoch
	token.Epoch = epoch
	kind := "ipv4-mapping"
	if request.Endpoint.Local.Addr().Is6() {
		kind = "ipv6-pinhole"
	}
	return r.Lease{External: external, Lifetime: lifetime, GatewayID: p.Server.String(), ResourceID: fmt.Sprintf("pcp:%x", token.Nonce), Kind: kind, Token: token, ServerEpoch: epoch, ServerRestarted: restarted}, nil
}
