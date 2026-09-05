package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestDatagramExchangeLocalSourceAndCancellation(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		body := make([]byte, 100)
		n, source, readErr := server.ReadFromUDP(body)
		if readErr == nil {
			_, _ = server.WriteToUDP(body[:n], source)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body, err := DatagramExchange(ctx, netip.MustParseAddr("127.0.0.1"), server.LocalAddr().(*net.UDPAddr).AddrPort(), []byte{1, 2, 3})
	if err != nil || len(body) != 3 {
		t.Fatal(body, err)
	}
	<-done
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = DatagramExchange(cancelled, netip.MustParseAddr("127.0.0.1"), server.LocalAddr().(*net.UDPAddr).AddrPort(), nil); err == nil {
		t.Fatal("cancel")
	}
}
func TestSOAPHTTPDoesNotFollowRedirectOrUseProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/redirect" {
			http.Redirect(w, req, "http://8.8.8.8/control", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte("<Envelope><NewExternalIPAddress>8.8.8.8</NewExternalIPAddress></Envelope>"))
	}))
	defer server.Close()
	req := request()
	req.Endpoint.Local = netip.MustParseAddrPort("127.0.0.1:4000")
	u := &UPnP{Service: Service{URL: server.URL, Gateway: netip.MustParseAddr("127.0.0.1"), Egress: "7", Type: WANIPv1}}
	result, err := u.call(context.Background(), req, "GetExternalIPAddress", nil)
	if err != nil || result["NewExternalIPAddress"] != "8.8.8.8" {
		t.Fatal(result, err)
	}
	u.Service.URL = server.URL + "/redirect"
	if _, err = u.call(context.Background(), req, "GetExternalIPAddress", nil); err == nil {
		t.Fatal("redirect accepted")
	}
}
