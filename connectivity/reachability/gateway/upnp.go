package gateway

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

const (
	WANIPv1         = "urn:schemas-upnp-org:service:WANIPConnection:1"
	WANIPv2         = "urn:schemas-upnp-org:service:WANIPConnection:2"
	WANPPP          = "urn:schemas-upnp-org:service:WANPPPConnection:1"
	WANIPv6Firewall = "urn:schemas-upnp-org:service:WANIPv6FirewallControl:1"
)

type Service struct {
	URL          string
	Type         string
	Gateway      netip.Addr
	Egress       string
	ControlLocal netip.Addr
}
type UPnP struct {
	Service Service
	HTTP    HTTPDo
}
type upnpToken struct{ PinholeID string }

func (u *UPnP) Create(ctx context.Context, request r.Request) (r.Lease, error) {
	if request.Endpoint.Egress != u.Service.Egress {
		return r.Lease{}, r.ErrUnavailable
	}
	if u.Service.Type == WANIPv6Firewall {
		return u.createPinhole(ctx, request)
	}
	if !request.Endpoint.Local.Addr().Is4() || request.Scope.Remote.IsValid() {
		return r.Lease{}, r.ErrUnavailable
	}
	if u.Service.Type != WANIPv1 && u.Service.Type != WANIPv2 && u.Service.Type != WANPPP {
		return r.Lease{}, r.ErrUnavailable
	}
	address, err := u.call(ctx, request, "GetExternalIPAddress", nil)
	if err != nil {
		return r.Lease{}, err
	}
	external, err := netip.ParseAddr(address["NewExternalIPAddress"])
	if err != nil || !external.Is4() || !r.PublicAddress(external) {
		return r.Lease{}, r.ErrInvalidResponse
	}
	port := request.Endpoint.Local.Port()
	args := mappingArgs(request, port)
	action := "AddPortMapping"
	if u.Service.Type == WANIPv2 {
		action = "AddAnyPortMapping"
	}
	result, err := u.call(ctx, request, action, args)
	if err != nil {
		return r.Lease{}, err
	}
	if action == "AddAnyPortMapping" {
		parsed, e := strconv.ParseUint(result["NewReservedPort"], 10, 16)
		if e != nil || parsed == 0 {
			return r.Lease{}, r.ErrInvalidResponse
		}
		port = uint16(parsed)
	}
	lease := r.Lease{External: netip.AddrPortFrom(external, port), Lifetime: request.Lifetime, GatewayID: u.Service.Gateway.String(), ResourceID: fmt.Sprintf("upnp:%d:%d", request.Endpoint.Protocol, port), Kind: "ipv4-mapping", Token: upnpToken{}}
	// Devices that silently install permanent mappings violate crash TTL ownership.
	// Verify the installed tuple/lifetime before publishing, and revoke on mismatch.
	result, err = u.call(ctx, request, "GetSpecificPortMappingEntry", portArgs(request, port))
	seconds, parseErr := strconv.ParseUint(result["NewLeaseDuration"], 10, 32)
	if err != nil || parseErr != nil || seconds == 0 || time.Duration(seconds)*time.Second > request.Lifetime || result["NewInternalClient"] != request.Endpoint.Local.Addr().String() || result["NewInternalPort"] != strconv.Itoa(int(request.Endpoint.Local.Port())) || result["NewEnabled"] != "1" {
		return r.Lease{}, revokeInvalid(ctx, u, request, lease)
	}
	lease.Lifetime = time.Duration(seconds) * time.Second
	return lease, nil
}
func (u *UPnP) Renew(ctx context.Context, request r.Request, lease r.Lease) (r.Lease, error) {
	if token, ok := lease.Token.(upnpToken); ok && token.PinholeID != "" {
		_, err := u.call(ctx, request, "UpdatePinhole", map[string]string{"UniqueID": token.PinholeID, "NewLeaseTime": seconds(request.Lifetime)})
		if err != nil {
			return r.Lease{}, err
		}
		lease.Lifetime = request.Lifetime
		return lease, nil
	}
	_, err := u.call(ctx, request, "AddPortMapping", mappingArgs(request, lease.External.Port()))
	if err != nil {
		return r.Lease{}, err
	}
	result, err := u.call(ctx, request, "GetSpecificPortMappingEntry", portArgs(request, lease.External.Port()))
	if err != nil {
		// An unavailable verification does not revoke a known unexpired lease.
		return r.Lease{}, err
	}
	lifetime, e := strconv.ParseUint(result["NewLeaseDuration"], 10, 32)
	if e != nil || lifetime == 0 || time.Duration(lifetime)*time.Second > request.Lifetime || result["NewInternalClient"] != request.Endpoint.Local.Addr().String() || result["NewInternalPort"] != strconv.Itoa(int(request.Endpoint.Local.Port())) || result["NewEnabled"] != "1" {
		return r.Lease{}, revokeInvalid(ctx, u, request, lease)
	}
	lease.Lifetime = time.Duration(lifetime) * time.Second
	return lease, nil
}
func (u *UPnP) Delete(ctx context.Context, request r.Request, lease r.Lease) error {
	if token, ok := lease.Token.(upnpToken); ok && token.PinholeID != "" {
		_, err := u.call(ctx, request, "DeletePinhole", map[string]string{"UniqueID": token.PinholeID})
		return err
	}
	_, err := u.call(ctx, request, "DeletePortMapping", portArgs(request, lease.External.Port()))
	return err
}
func (u *UPnP) createPinhole(ctx context.Context, request r.Request) (r.Lease, error) {
	if !request.Endpoint.Local.Addr().Is6() || !r.PublicAddress(request.Endpoint.Local.Addr()) {
		return r.Lease{}, r.ErrUnavailable
	}
	status, err := u.call(ctx, request, "GetFirewallStatus", nil)
	if err != nil {
		return r.Lease{}, err
	}
	if status["FirewallEnabled"] != "1" || status["InboundPinholeAllowed"] != "1" {
		return r.Lease{}, r.ErrUnavailable
	}
	remoteHost := ""
	remotePort := "0"
	if request.Scope.Remote.IsValid() {
		remoteHost = request.Scope.Remote.Addr().String()
		remotePort = strconv.Itoa(int(request.Scope.Remote.Port()))
	}
	result, err := u.call(ctx, request, "AddPinhole", map[string]string{"RemoteHost": remoteHost, "RemotePort": remotePort, "InternalClient": request.Endpoint.Local.Addr().String(), "InternalPort": strconv.Itoa(int(request.Endpoint.Local.Port())), "Protocol": strconv.Itoa(int(request.Endpoint.Protocol)), "LeaseTime": seconds(request.Lifetime)})
	if err != nil {
		return r.Lease{}, err
	}
	id := result["UniqueID"]
	if _, err = strconv.ParseUint(id, 10, 16); err != nil {
		return r.Lease{}, r.ErrInvalidResponse
	}
	return r.Lease{External: request.Endpoint.Local, Lifetime: request.Lifetime, GatewayID: u.Service.Gateway.String(), ResourceID: "pinhole:" + id, Kind: "ipv6-pinhole", Token: upnpToken{PinholeID: id}}, nil
}
func seconds(lifetime time.Duration) string {
	return strconv.FormatInt(int64(lifetime/time.Second), 10)
}
func portArgs(request r.Request, port uint16) map[string]string {
	protocol := "UDP"
	if request.Endpoint.Protocol == r.TCP {
		protocol = "TCP"
	}
	return map[string]string{"NewRemoteHost": "", "NewExternalPort": strconv.Itoa(int(port)), "NewProtocol": protocol}
}
func mappingArgs(request r.Request, port uint16) map[string]string {
	args := portArgs(request, port)
	args["NewInternalPort"] = strconv.Itoa(int(request.Endpoint.Local.Port()))
	args["NewInternalClient"] = request.Endpoint.Local.Addr().String()
	args["NewEnabled"] = "1"
	args["NewPortMappingDescription"] = "WindShare"
	args["NewLeaseDuration"] = seconds(request.Lifetime)
	return args
}
