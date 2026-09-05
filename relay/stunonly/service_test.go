package stunonly

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServiceOwnsOnlyItsListenersAndReportsEveryFailure(t *testing.T) {
	var packet net.PacketConn
	var admin net.Listener
	listeners := Listeners{
		Packet: func(network, address string) (net.PacketConn, error) {
			if address == "failed" {
				return nil, errors.New("bind failed")
			}
			var err error
			packet, err = net.ListenPacket("udp4", "127.0.0.1:0")
			return packet, err
		},
		TCP: func(string, string) (net.Listener, error) {
			var err error
			admin, err = net.Listen("tcp4", "127.0.0.1:0")
			return admin, err
		},
	}
	service := StartService(context.Background(), ServiceConfig{UDPAddresses: []string{"failed", "working"}, AdminAddress: "admin"}, listeners, t.Logf)
	defer service.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + admin.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing bind failure from health: %d", response.StatusCode)
	}
	service.Close()
	service.Close()
	if err := packet.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("UDP survived close: %v", err)
	}
	if _, err := admin.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("admin survived close: %v", err)
	}
}

func TestServiceDefaultsAndInvalidConfigurationRemainIsolated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := StartService(ctx, ServiceConfig{UDPAddresses: []string{"127.0.0.1:0"}, AdminAddress: "127.0.0.1:0"}, Listeners{}, nil)
	service.Close()
	var events []string
	service = StartService(context.Background(), ServiceConfig{
		UDPAddresses: []string{"127.0.0.1:0"}, AdminAddress: "invalid",
		Limits: Config{RequestsPerSecond: -1},
	}, Listeners{}, func(message string, _ ...any) { events = append(events, message) })
	service.Close()
	if len(events) != 2 || !strings.Contains(events[0], "stun_listener_unavailable") || !strings.Contains(events[1], "stun_admin_unavailable") {
		t.Fatal(events)
	}
	disabled := StartService(context.Background(), ServiceConfig{}, Listeners{
		Packet: func(string, string) (net.PacketConn, error) { t.Fatal("disabled UDP bind"); return nil, nil },
		TCP:    func(string, string) (net.Listener, error) { t.Fatal("disabled admin bind"); return nil, nil },
	}, nil)
	disabled.Close()
}
