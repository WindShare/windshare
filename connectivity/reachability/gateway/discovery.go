package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

const maxServices = 8
const maxDiscoveryReplies = 16
const ssdpAddress = "239.255.255.250:1900"
const ssdpSearch = "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"

func EgressID(index int) string { return strconv.Itoa(index) }

// Defaults uses only OS-reported default routers. DHCP/configured PCP groups
// can replace this fallback using DiscoverPCP; nothing scans guessed subnets.
func Defaults(snapshot networkstate.Snapshot) []r.Gateway {
	var gateways []r.Gateway
	for _, route := range snapshot.Routes() {
		if !route.Gateway.IsValid() || route.Gateway.IsUnspecified() {
			continue
		}
		egress := EgressID(route.InterfaceIndex)
		server := netip.AddrPortFrom(route.Gateway, controlPort)
		gateways = append(gateways, &PCP{Server: server, Egress: egress})
		if route.Gateway.Is4() {
			var controlLocal netip.Addr
			for _, address := range snapshot.Addresses() {
				if address.InterfaceIndex == route.InterfaceIndex && address.IP.Is4() {
					controlLocal = address.IP
					break
				}
			}
			gateways = append(gateways, &NATPMP{Server: server, Egress: egress}, &AutoUPnP{Gateway: route.Gateway, Egress: egress, ControlLocal: controlLocal})
		}
	}
	return gateways
}

type AutoUPnP struct {
	Gateway      netip.Addr
	Egress       string
	ControlLocal netip.Addr
	Discover     func(context.Context, netip.Addr, netip.Addr, string) ([]Service, error)
	HTTP         HTTPDo
}
type autoToken struct {
	service Service
	token   any
}

func (a *AutoUPnP) Create(ctx context.Context, request r.Request) (r.Lease, error) {
	if err := ctx.Err(); err != nil {
		return r.Lease{}, err
	}
	if request.Endpoint.Egress != a.Egress {
		return r.Lease{}, r.ErrUnavailable
	}
	discover := a.Discover
	if discover == nil {
		discover = DiscoverUPnP
	}
	local := request.Endpoint.Local.Addr()
	if a.ControlLocal.IsValid() {
		local = a.ControlLocal
	}
	services, err := discover(ctx, local, a.Gateway, a.Egress)
	if err != nil {
		return r.Lease{}, err
	}
	for _, service := range services {
		service.ControlLocal = local
		client := &UPnP{Service: service, HTTP: a.HTTP}
		lease, err := client.Create(ctx, request)
		if err == nil {
			lease.Token = autoToken{service: service, token: lease.Token}
			return lease, nil
		}
	}
	return r.Lease{}, r.ErrUnavailable
}
func (a *AutoUPnP) Renew(ctx context.Context, request r.Request, lease r.Lease) (r.Lease, error) {
	token, ok := lease.Token.(autoToken)
	if !ok {
		return r.Lease{}, r.ErrInvalid
	}
	lease.Token = token.token
	renewed, err := (&UPnP{Service: token.service, HTTP: a.HTTP}).Renew(ctx, request, lease)
	if err == nil {
		renewed.Token = autoToken{service: token.service, token: renewed.Token}
	}
	return renewed, err
}
func (a *AutoUPnP) Delete(ctx context.Context, request r.Request, lease r.Lease) error {
	token, ok := lease.Token.(autoToken)
	if !ok {
		return r.ErrInvalid
	}
	lease.Token = token.token
	return (&UPnP{Service: token.service, HTTP: a.HTTP}).Delete(ctx, request, lease)
}
func DiscoverUPnP(ctx context.Context, local, gateway netip.Addr, egress string) ([]Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !local.Is4() || !gateway.Is4() {
		return nil, r.ErrUnavailable
	}
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(time.Second)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		deadline = bound
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err = conn.WriteToUDPAddrPort([]byte(ssdpSearch), netip.MustParseAddrPort(ssdpAddress)); err != nil {
		return nil, err
	}
	buffer := make([]byte, 4096)
	for range maxDiscoveryReplies {
		n, source, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return nil, err
		}
		if source.Addr().Unmap() != gateway.Unmap() {
			continue
		}
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buffer[:n])), nil)
		if err != nil {
			continue
		}
		location := response.Header.Get("Location")
		_ = response.Body.Close()
		if !validControlURL(location, gateway) {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		if err != nil {
			return nil, err
		}
		client, transport := gatewayHTTP(local)
		description, err := client.Do(request)
		transport.CloseIdleConnections()
		if err != nil {
			return nil, err
		}
		services, parseErr := ParseDescription(description.Body, location, gateway, egress)
		_ = description.Body.Close()
		if description.StatusCode != http.StatusOK {
			return nil, r.ErrUnavailable
		}
		return services, parseErr
	}
	return nil, r.ErrUnavailable
}
func ParseDescription(reader io.Reader, location string, gateway netip.Addr, egress string) ([]Service, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxXMLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxXMLBytes {
		return nil, r.ErrInvalidResponse
	}
	base, err := url.Parse(location)
	if err != nil || !validControlURL(location, gateway) {
		return nil, r.ErrInvalidResponse
	}
	parser := descriptionParser{base: base, gateway: gateway, egress: egress}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return parser.services, nil
		}
		if err != nil {
			return nil, r.ErrInvalidResponse
		}
		if err := parser.consume(token); err != nil {
			return nil, err
		}
	}
}

// The token state keeps the size/depth limits and service boundaries under one
// owner; URL resolution happens only after a complete supported service.
type descriptionParser struct {
	base      *url.URL
	gateway   netip.Addr
	egress    string
	depth     int
	inService bool
	field     string
	service   Service
	services  []Service
}

func (p *descriptionParser) consume(token xml.Token) error {
	switch value := token.(type) {
	case xml.StartElement:
		p.depth++
		if p.depth > maxXMLDepth {
			return r.ErrInvalidResponse
		}
		p.field = value.Name.Local
		if p.field == "service" {
			if p.inService {
				return r.ErrInvalidResponse
			}
			p.inService = true
			p.service = Service{Gateway: p.gateway, Egress: p.egress}
		}
	case xml.CharData:
		if !p.inService {
			return nil
		}
		switch p.field {
		case "serviceType":
			p.service.Type += strings.TrimSpace(string(value))
		case "controlURL":
			p.service.URL += strings.TrimSpace(string(value))
		}
	case xml.EndElement:
		if value.Name.Local == "service" {
			p.inService = false
			if err := p.finishService(); err != nil {
				return err
			}
		}
		p.field = ""
		p.depth--
	case xml.Directive:
		return r.ErrInvalidResponse
	}
	return nil
}

func (p *descriptionParser) finishService() error {
	switch p.service.Type {
	case WANIPv1, WANIPv2, WANPPP, WANIPv6Firewall:
	default:
		return nil
	}
	relative, err := url.Parse(p.service.URL)
	if err != nil {
		return r.ErrInvalidResponse
	}
	p.service.URL = p.base.ResolveReference(relative).String()
	if !validControlURL(p.service.URL, p.gateway) {
		return r.ErrInvalidResponse
	}
	p.services = append(p.services, p.service)
	if len(p.services) > maxServices {
		return r.ErrInvalidResponse
	}
	return nil
}
