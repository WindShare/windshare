package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

type NATPMP struct {
	Server   netip.AddrPort
	Egress   string
	Exchange Exchange
}
type pmpToken struct{ Epoch uint32 }

func (p *NATPMP) Create(ctx context.Context, request r.Request) (r.Lease, error) {
	return p.mapping(ctx, request, r.Lease{}, false)
}
func (p *NATPMP) Renew(ctx context.Context, request r.Request, lease r.Lease) (r.Lease, error) {
	return p.mapping(ctx, request, lease, false)
}
func (p *NATPMP) Delete(ctx context.Context, request r.Request, lease r.Lease) error {
	request.Lifetime = 0
	_, err := p.mapping(ctx, request, lease, true)
	return err
}
func (p *NATPMP) mapping(ctx context.Context, request r.Request, previous r.Lease, deleting bool) (r.Lease, error) {
	if err := ctx.Err(); err != nil {
		return r.Lease{}, err
	}
	if request.Endpoint.Egress != p.Egress || !request.Endpoint.Local.Addr().Is4() || request.Scope.Remote.IsValid() || !p.Server.Addr().Is4() {
		return r.Lease{}, r.ErrUnavailable
	}
	exchange := p.Exchange
	if exchange == nil {
		exchange = DatagramExchange
	}
	var externalIP netip.Addr
	var publicEpoch uint32
	if !deleting {
		response, err := exchange(ctx, request.Endpoint.Local.Addr(), p.Server, []byte{0, 0})
		if err != nil {
			return r.Lease{}, err
		}
		if err = validatePMP(response, 128, 12); err != nil {
			return r.Lease{}, err
		}
		externalIP = netip.AddrFrom4([4]byte(response[8:12]))
		publicEpoch = binary.BigEndian.Uint32(response[4:8])
		if !r.PublicAddress(externalIP) {
			return r.Lease{}, r.ErrInvalidResponse
		}
	}
	opcode := byte(1)
	if request.Endpoint.Protocol == r.TCP {
		opcode = 2
	}
	body := make([]byte, 12)
	body[1] = opcode
	binary.BigEndian.PutUint16(body[4:6], request.Endpoint.Local.Port())
	port := request.Endpoint.Local.Port()
	if previous.External.IsValid() {
		port = previous.External.Port()
	}
	binary.BigEndian.PutUint16(body[6:8], port)
	binary.BigEndian.PutUint32(body[8:12], uint32(request.Lifetime/time.Second))
	if err := ctx.Err(); err != nil {
		return r.Lease{}, err
	}
	response, err := exchange(ctx, request.Endpoint.Local.Addr(), p.Server, body)
	if err != nil {
		return r.Lease{}, err
	}
	if err = validatePMP(response, opcode+128, 16); err != nil {
		return r.Lease{}, err
	}
	if binary.BigEndian.Uint16(response[8:10]) != request.Endpoint.Local.Port() {
		return r.Lease{}, r.ErrInvalidResponse
	}
	lifetime := time.Duration(binary.BigEndian.Uint32(response[12:16])) * time.Second
	if deleting {
		if lifetime != 0 {
			return r.Lease{}, r.ErrInvalidResponse
		}
		return r.Lease{}, nil
	}
	epoch := binary.BigEndian.Uint32(response[4:8])
	previousToken, _ := previous.Token.(pmpToken)
	if epoch < publicEpoch {
		return r.Lease{}, fmt.Errorf("%w: %w: NAT-PMP server epoch reset", r.ErrLeaseLost, r.ErrUnavailable)
	}
	external := netip.AddrPortFrom(externalIP, binary.BigEndian.Uint16(response[10:12]))
	if external.Port() == 0 || lifetime <= 0 || lifetime > request.Lifetime {
		if lifetime > 0 {
			return r.Lease{}, revokeInvalid(ctx, p, request, r.Lease{External: external, Token: pmpToken{Epoch: epoch}})
		}
		return r.Lease{}, fmt.Errorf("%w: %w", r.ErrLeaseLost, r.ErrInvalidResponse)
	}
	return r.Lease{External: external, Lifetime: lifetime, GatewayID: p.Server.String(), ResourceID: fmt.Sprintf("pmp:%d:%d", request.Endpoint.Protocol, request.Endpoint.Local.Port()), Kind: "ipv4-mapping", Token: pmpToken{Epoch: epoch}, ServerEpoch: epoch, ServerRestarted: previousToken.Epoch > 0 && epoch < previousToken.Epoch}, nil
}
func validatePMP(response []byte, opcode byte, length int) error {
	if len(response) != length || response[0] != 0 || response[1] != opcode {
		return r.ErrInvalidResponse
	}
	if result := binary.BigEndian.Uint16(response[2:4]); result != 0 {
		return fmt.Errorf("%w: NAT-PMP result %d", r.ErrUnavailable, result)
	}
	return nil
}
