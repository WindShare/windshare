package browsermatrixpion

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v3"

	"github.com/windshare/windshare/internal/testnetwork"
)

func TestRealSTUNProberRequiresServerReflexiveBindingResponse(t *testing.T) {
	uri, stop := startLocalSTUNServer(t, net.IPv4(198, 51, 100, 20))
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := (RealSTUNProber{}).Probe(ctx, uri); err == nil {
		t.Fatal("loopback STUN endpoint accepted as public authority")
	}
	if err := testSTUNProber().Probe(ctx, uri); err != nil {
		t.Fatal(err)
	}

	nonPublicURI, stopNonPublic := startLocalSTUNServer(t, net.IPv4zero)
	defer stopNonPublic()
	if err := testSTUNProber().Probe(ctx, nonPublicURI); err == nil {
		t.Fatal("unspecified mapped address accepted as server-reflexive")
	}
}

func testSTUNProber() RealSTUNProber {
	return RealSTUNProber{AddressPolicy: func(address netip.Addr) bool {
		return address.IsValid() && !address.IsUnspecified()
	}}
}

func TestRealSTUNProberRejectsInvalidAndUnreachableEndpoint(t *testing.T) {
	for _, endpoint := range []string{"not-stun", "turn:127.0.0.1:3478", "stun:127.0.0.1:1?transport=tcp"} {
		if err := (RealSTUNProber{}).Probe(context.Background(), endpoint); err == nil {
			t.Fatalf("invalid endpoint accepted: %q", endpoint)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (RealSTUNProber{}).Probe(ctx, "stun:127.0.0.1:9"); err == nil {
		t.Fatal("canceled probe succeeded")
	}
}

func TestPublicSTUNAddressRejectsPrivateCarrierAndDocumentationRanges(t *testing.T) {
	if !publicSTUNAddress(netip.MustParseAddr("8.8.8.8")) ||
		!publicSTUNAddress(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("public unicast address was rejected")
	}
	for _, value := range []string{
		"10.0.0.1", "100.64.0.1", "127.0.0.1", "192.0.2.1", "203.0.113.1",
		"::1", "2001:db8::1", "fd00::1", "fe80::1",
	} {
		if publicSTUNAddress(netip.MustParseAddr(value)) {
			t.Fatalf("non-public address accepted: %s", value)
		}
	}
}

func startLocalSTUNServer(t *testing.T, mappedIP net.IP) (string, func()) {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		length, remote, readErr := listener.ReadFromUDP(buffer)
		if readErr != nil {
			return
		}
		request := &stun.Message{Raw: append([]byte(nil), buffer[:length]...)}
		if request.Decode() != nil {
			return
		}
		response := stun.MustBuild(
			stun.NewTransactionIDSetter(request.TransactionID),
			stun.BindingSuccess,
			&stun.XORMappedAddress{IP: mappedIP, Port: 54321},
			stun.Fingerprint,
		)
		_, _ = listener.WriteToUDP(response.Raw, remote)
	}()
	stop := func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("local STUN server did not retire")
		}
	}
	return fmt.Sprintf("stun:127.0.0.1:%d", listener.LocalAddr().(*net.UDPAddr).Port), stop
}
