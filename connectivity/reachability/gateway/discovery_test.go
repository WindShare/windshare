package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

func TestPCPDiscoveryGroupsAndPrecedence(t *testing.T) {
	input := PCPDiscovery{Egress: "7", DHCPv4: []byte{8, 192, 168, 1, 1, 192, 168, 1, 2, 4, 10, 0, 0, 1}}
	servers, err := DiscoverPCP(input)
	if err != nil || len(servers) != 2 || len(servers[0].Addresses) != 2 {
		t.Fatal(servers, err)
	}
	v6 := netip.MustParseAddr("2001:4860::1").As16()
	mapped := netip.MustParseAddr("192.168.1.1").As16()
	input.DHCPv4 = nil
	input.DHCPv6 = [][]byte{v6[:], mapped[:]}
	servers, err = DiscoverPCP(input)
	if err != nil || len(servers) != 2 || !servers[1].Addresses[0].Is4() {
		t.Fatal(servers, err)
	}
	input.Configured = []PCPServer{{Egress: "7", Addresses: []netip.Addr{netip.MustParseAddr("192.168.1.3")}}}
	servers, err = DiscoverPCP(input)
	if err != nil || len(servers) != 1 || servers[0].Addresses[0].String() != "192.168.1.3" {
		t.Fatal(servers, err)
	}
	input.Configured[0].Addresses[0] = netip.MustParseAddr("192.168.1.4")
	if servers[0].Addresses[0].String() != "192.168.1.3" {
		t.Fatal("mutable config")
	}
	input = PCPDiscovery{Egress: "7", DefaultRouters: []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("127.0.0.1")}}
	servers, err = DiscoverPCP(input)
	if err != nil || len(servers) != 1 {
		t.Fatal(servers, err)
	}
	input.DHCPv4 = []byte{4, 224, 0, 0, 1}
	servers, err = DiscoverPCP(input)
	if err != nil || len(servers) != 1 {
		t.Fatal("discard multicast then fallback")
	}
	for _, bad := range []PCPDiscovery{
		{DHCPv4: []byte{0}}, {DHCPv4: []byte{3, 1, 2, 3}}, {DHCPv4: []byte{8, 1, 2, 3, 4}}, {DHCPv6: [][]byte{{1}}},
		{Egress: "7", Configured: []PCPServer{{Egress: "8", Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}}},
		{Configured: []PCPServer{{Addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}}},
	} {
		if _, err := DiscoverPCP(bad); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestDescriptionBoundsAndTrust(t *testing.T) {
	raw := "<root><device><serviceList><service><serviceType>" + WANIPv2 + "</serviceType><controlURL>/ctl</controlURL></service><service><serviceType>" + WANIPv6Firewall + "</serviceType><controlURL>pin</controlURL></service></serviceList></device></root>"
	gateway := netip.MustParseAddr("192.168.1.1")
	services, err := ParseDescription(strings.NewReader(raw), "http://192.168.1.1/root.xml", gateway, "7")
	if err != nil || len(services) != 2 || services[0].URL != "http://192.168.1.1/ctl" || services[1].URL != "http://192.168.1.1/pin" {
		t.Fatal(services, err)
	}
	for _, bad := range []string{
		strings.ReplaceAll(raw, "/ctl", "http://8.8.8.8/ctl"), strings.Repeat("<a>", maxXMLDepth+1),
		strings.Repeat("x", maxXMLBytes+1), "<root>", "<!DOCTYPE x><root/>",
		"<root>" + strings.Repeat("<service><serviceType>"+WANIPv1+"</serviceType><controlURL>/ctl</controlURL></service>", maxServices+1) + "</root>",
		"<root><service><service></service></service></root>",
	} {
		if _, err := ParseDescription(strings.NewReader(bad), "http://192.168.1.1/root.xml", gateway, "7"); err == nil {
			t.Fatal("accepted malformed description")
		}
	}
	if _, err := ParseDescription(strings.NewReader(raw), "http://8.8.8.8/root.xml", gateway, "7"); err == nil {
		t.Fatal("untrusted location")
	}
}
func TestGatewayDefaultsAndAutomaticUPnP(t *testing.T) {
	observer := networkstate.Observer{}
	snapshot, _ := observer.Observe(networkstate.State{
		Addresses: []networkstate.Address{{IP: netip.MustParseAddr("192.168.1.2"), InterfaceIndex: 7}},
		Routes:    []networkstate.Route{{InterfaceIndex: 7, Family: 4, Gateway: netip.MustParseAddr("192.168.1.1")}, {InterfaceIndex: 7, Family: 6, Gateway: netip.MustParseAddr("fe80::1%7")}, {InterfaceIndex: 8, Family: 4}},
	}, time.Now())
	defaults := Defaults(snapshot)
	if len(defaults) != 4 || EgressID(7) != "7" {
		t.Fatal(defaults)
	}
	auto := &AutoUPnP{Gateway: netip.MustParseAddr("192.168.1.1"), Egress: "7", ControlLocal: netip.MustParseAddr("192.168.1.2"),
		Discover: func(_ context.Context, local, gw netip.Addr, egress string) ([]Service, error) {
			if local.String() != "192.168.1.2" || egress != "7" {
				t.Fatal("egress affinity")
			}
			return []Service{service("unknown"), service(WANIPv6Firewall)}, nil
		},
		HTTP: func(req *http.Request) (*http.Response, error) {
			action := req.Header.Get("SOAPAction")
			if strings.Contains(action, "GetFirewallStatus") {
				return soapResponse("<FirewallEnabled>1</FirewallEnabled><InboundPinholeAllowed>1</InboundPinholeAllowed>"), nil
			}
			if strings.Contains(action, "AddPinhole") {
				return soapResponse("<UniqueID>1</UniqueID>"), nil
			}
			return soapResponse(""), nil
		},
	}
	req := request()
	req.Endpoint.Local = netip.MustParseAddrPort("[2606:4700::1]:4000")
	lease, err := auto.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = auto.Renew(context.Background(), req, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = auto.Delete(context.Background(), req, lease); err != nil {
		t.Fatal(err)
	}
	if _, err = auto.Renew(context.Background(), req, r.Lease{}); err == nil {
		t.Fatal("invalid token")
	}
	if err = auto.Delete(context.Background(), req, r.Lease{}); err == nil {
		t.Fatal("invalid delete token")
	}
	auto.Discover = func(context.Context, netip.Addr, netip.Addr, string) ([]Service, error) {
		return nil, errors.New("discovery")
	}
	if _, err = auto.Create(context.Background(), req); err == nil {
		t.Fatal("discovery failure")
	}
	auto.Egress = "other"
	if _, err = auto.Create(context.Background(), req); err == nil {
		t.Fatal("egress")
	}
	if _, err = DiscoverUPnP(context.Background(), netip.MustParseAddr("::1"), netip.MustParseAddr("192.168.1.1"), "7"); err == nil {
		t.Fatal("IPv6 SSDP")
	}
}
