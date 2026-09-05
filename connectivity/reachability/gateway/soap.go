package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	r "github.com/windshare/windshare/connectivity/reachability"
)

const maxXMLBytes = 64 * 1024
const maxXMLDepth = 16
const soapNoMapping = "714"
const soapNoPinhole = "704"

type HTTPDo func(*http.Request) (*http.Response, error)

func validControlURL(raw string, gateway netip.Addr) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	host, err := netip.ParseAddr(parsed.Hostname())
	return err == nil && host.Unmap() == gateway.Unmap() && !host.IsUnspecified() && !host.IsMulticast()
}
func gatewayHTTP(local netip.Addr) (*http.Client, *http.Transport) {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(local.AsSlice()), Zone: local.Zone()}, Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, transport
}
func (u *UPnP) call(ctx context.Context, request r.Request, action string, args map[string]string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validControlURL(u.Service.URL, u.Service.Gateway) || request.Endpoint.Egress != u.Service.Egress {
		return nil, r.ErrUnavailable
	}
	var body bytes.Buffer
	body.WriteString("<?xml version=\"1.0\"?><s:Envelope xmlns:s=\"http://schemas.xmlsoap.org/soap/envelope/\" s:encodingStyle=\"http://schemas.xmlsoap.org/soap/encoding/\"><s:Body><u:" + action + " xmlns:u=\"" + u.Service.Type + "\">")
	keys := soapArgumentOrder(action)
	for _, key := range keys {
		body.WriteString("<" + key + ">")
		_ = xml.EscapeText(&body, []byte(args[key]))
		body.WriteString("</" + key + ">")
	}
	body.WriteString("</u:" + action + "></s:Body></s:Envelope>")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Service.URL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", "\""+u.Service.Type+"#"+action+"\"")
	do := u.HTTP
	if do == nil {
		local := request.Endpoint.Local.Addr()
		if u.Service.ControlLocal.IsValid() {
			local = u.Service.ControlLocal
		}
		client, transport := gatewayHTTP(local)
		defer transport.CloseIdleConnections()
		do = client.Do
	}
	response, err := do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	result, err := xmlLeaves(response.Body)
	if err != nil {
		return nil, err
	}
	// These SOAP faults explicitly report that the addressed resource is absent.
	// Other faults and transport failures do not revoke an existing finite lease.
	if (action == "GetSpecificPortMappingEntry" && result["errorCode"] == soapNoMapping) ||
		(action == "UpdatePinhole" && result["errorCode"] == soapNoPinhole) {
		return nil, fmt.Errorf("%w: UPnP SOAP %s", r.ErrLeaseLost, result["errorCode"])
	}
	if response.StatusCode != http.StatusOK || result["errorCode"] != "" {
		return nil, fmt.Errorf("%w: UPnP HTTP %d SOAP %s", r.ErrUnavailable, response.StatusCode, result["errorCode"])
	}
	return result, nil
}
func xmlLeaves(reader io.Reader) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxXMLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxXMLBytes {
		return nil, r.ErrInvalidResponse
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	result := make(map[string]string)
	var stack []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, r.ErrInvalidResponse
		}
		switch value := token.(type) {
		case xml.StartElement:
			stack = append(stack, value.Name.Local)
			if len(stack) > maxXMLDepth {
				return nil, r.ErrInvalidResponse
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, r.ErrInvalidResponse
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				text := strings.TrimSpace(string(value))
				if text != "" {
					result[stack[len(stack)-1]] += text
				}
			}
		case xml.Directive:
			return nil, r.ErrInvalidResponse
		}
	}
	if len(stack) != 0 {
		return nil, r.ErrInvalidResponse
	}
	return result, nil
}
