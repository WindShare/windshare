package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

func soapResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<Envelope><Body>" + body + "</Body></Envelope>"))}
}
func service(kind string) Service {
	return Service{URL: "http://192.168.1.1/control", Type: kind, Gateway: netip.MustParseAddr("192.168.1.1"), Egress: "7"}
}
func TestUPnPMappingLifecycle(t *testing.T) {
	for _, kind := range []string{WANIPv1, WANIPv2, WANPPP} {
		req := request()
		var actions []string
		client := &UPnP{Service: service(kind), HTTP: func(req *http.Request) (*http.Response, error) {
			action := req.Header.Get("SOAPAction")
			actions = append(actions, action)
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(action, "Add") && strings.Index(string(body), "<NewExternalPort>") > strings.Index(string(body), "<NewProtocol>") {
				t.Fatal("SOAP order")
			}
			switch {
			case strings.Contains(action, "GetExternalIPAddress"):
				return soapResponse("<NewExternalIPAddress>8.8.8.8</NewExternalIPAddress>"), nil
			case strings.Contains(action, "AddAnyPortMapping"):
				return soapResponse("<NewReservedPort>55000</NewReservedPort>"), nil
			case strings.Contains(action, "GetSpecificPortMappingEntry"):
				return soapResponse("<NewInternalClient>192.168.1.2</NewInternalClient><NewInternalPort>4000</NewInternalPort><NewEnabled>1</NewEnabled><NewLeaseDuration>60</NewLeaseDuration>"), nil
			default:
				return soapResponse(""), nil
			}
		}}
		lease, err := client.Create(context.Background(), req)
		if err != nil {
			t.Fatal(kind, err)
		}
		port := uint16(4000)
		if kind == WANIPv2 {
			port = 55000
		}
		if lease.External.Port() != port || lease.Lifetime != time.Minute {
			t.Fatal(lease)
		}
		lease, err = client.Renew(context.Background(), req, lease)
		if err != nil {
			t.Fatal(err)
		}
		if err = client.Delete(context.Background(), req, lease); err != nil {
			t.Fatal(err)
		}
		if len(actions) != 6 {
			t.Fatal(actions)
		}
	}
}
func TestUPnPPinholeWildcardRestrictedLifecycle(t *testing.T) {
	for _, remote := range []string{"", "[2001:4860::1]:5000"} {
		req := request()
		req.Endpoint.Local = netip.MustParseAddrPort("[2606:4700::1]:4000")
		if remote != "" {
			req.Scope.Remote = netip.MustParseAddrPort(remote)
		}
		var actions []string
		client := &UPnP{Service: service(WANIPv6Firewall), HTTP: func(httpReq *http.Request) (*http.Response, error) {
			action := httpReq.Header.Get("SOAPAction")
			actions = append(actions, action)
			if strings.Contains(action, "GetFirewallStatus") {
				return soapResponse("<FirewallEnabled>1</FirewallEnabled><InboundPinholeAllowed>1</InboundPinholeAllowed>"), nil
			}
			if strings.Contains(action, "AddPinhole") {
				body, _ := io.ReadAll(httpReq.Body)
				if remote != "" && !strings.Contains(string(body), "<RemotePort>5000</RemotePort>") {
					t.Fatal("scope lost")
				}
				return soapResponse("<UniqueID>77</UniqueID>"), nil
			}
			return soapResponse(""), nil
		}}
		lease, err := client.Create(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if lease.External != req.Endpoint.Local || lease.ResourceID != "pinhole:77" {
			t.Fatal(lease)
		}
		lease, err = client.Renew(context.Background(), req, lease)
		if err != nil {
			t.Fatal(err)
		}
		if err = client.Delete(context.Background(), req, lease); err != nil {
			t.Fatal(err)
		}
		if len(actions) != 4 {
			t.Fatal(actions)
		}
	}
}
func TestUPnPPermanentAndMalformedRejected(t *testing.T) {
	req := request()
	deleted := false
	client := &UPnP{Service: service(WANIPv2), HTTP: func(req *http.Request) (*http.Response, error) {
		action := req.Header.Get("SOAPAction")
		switch {
		case strings.Contains(action, "GetExternalIPAddress"):
			return soapResponse("<NewExternalIPAddress>8.8.8.8</NewExternalIPAddress>"), nil
		case strings.Contains(action, "AddAnyPortMapping"):
			return soapResponse("<NewReservedPort>55000</NewReservedPort>"), nil
		case strings.Contains(action, "DeletePortMapping"):
			deleted = true
			return soapResponse(""), nil
		default:
			return soapResponse("<NewLeaseDuration>0</NewLeaseDuration>"), nil
		}
	}}
	if _, err := client.Create(context.Background(), req); err == nil || !deleted {
		t.Fatal("permanent mapping accepted/leaked")
	}
	for _, response := range []string{"<NewExternalIPAddress>10.0.0.1</NewExternalIPAddress>", "<errorCode>606</errorCode>", "<bad>"} {
		client.HTTP = func(*http.Request) (*http.Response, error) { return soapResponse(response), nil }
		if _, err := client.Create(context.Background(), req); err == nil {
			t.Fatal(response)
		}
	}
	client.Service.URL = "http://8.8.8.8/control"
	if _, err := client.Create(context.Background(), req); err == nil {
		t.Fatal("untrusted control")
	}
	client.Service = service(WANIPv6Firewall)
	if _, err := client.Create(context.Background(), req); err == nil {
		t.Fatal("IPv4 pinhole")
	}
	req.Endpoint.Local = netip.MustParseAddrPort("[2606:4700::1]:4000")
	client.HTTP = func(*http.Request) (*http.Response, error) {
		return soapResponse("<FirewallEnabled>1</FirewallEnabled><InboundPinholeAllowed>0</InboundPinholeAllowed>"), nil
	}
	if _, err := client.Create(context.Background(), req); err == nil {
		t.Fatal("pinhole disallowed")
	}
	client.Service = service("unknown")
	if _, err := client.Create(context.Background(), request()); err == nil {
		t.Fatal("unknown service")
	}
}
func TestXMLBoundsAndControlURLs(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", maxXMLBytes+1), strings.Repeat("<a>", maxXMLDepth+1), "<a>", "<!DOCTYPE foo><a/>"} {
		if _, err := xmlLeaves(strings.NewReader(body)); err == nil {
			t.Fatal("accepted malformed/oversized XML")
		}
	}
	valid := []string{"http://192.168.1.1/control", "http://192.168.1.1:5000/a"}
	for _, raw := range valid {
		if !validControlURL(raw, netip.MustParseAddr("192.168.1.1")) {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{"https://192.168.1.1/a", "http://router/a", "http://user@192.168.1.1/a", "http://192.168.1.1/a#f", "http://8.8.8.8/"} {
		if validControlURL(raw, netip.MustParseAddr("192.168.1.1")) {
			t.Fatal(raw)
		}
	}
	for _, protocol := range []r.Protocol{r.UDP, r.TCP} {
		req := request()
		req.Endpoint.Protocol = protocol
		if fmt.Sprint(mappingArgs(req, 55000)["NewExternalPort"]) != "55000" {
			t.Fatal("port")
		}
	}
}
