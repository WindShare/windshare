package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/windshare/windshare/relay/stunonly"
)

func TestSTUNFlags(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	options := registerSTUNFlags(flags)
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	config := options.config()
	if len(config.UDPAddresses) != 1 || config.UDPAddresses[0] != ":3478" || config.AdminAddress != "127.0.0.1:8081" {
		t.Fatalf("defaults: %+v", config)
	}
	if err := flags.Parse([]string{"-stun-udp", " :3478, , :443 ", "-stun-admin", "", "-stun-max-sources", "5"}); err != nil {
		t.Fatal(err)
	}
	config = options.config()
	if len(config.UDPAddresses) != 2 || config.UDPAddresses[1] != ":443" || config.AdminAddress != "" || config.Limits.MaximumSources != 5 {
		t.Fatal(config)
	}
}

func TestRelayAndSTUNShareProcessWithIndependentFailures(t *testing.T) {
	for _, scenario := range []string{"healthy", "bind-failure", "admin-failure", "runtime-failure", "invalid-limits", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var packets []net.PacketConn
			var admin net.Listener
			listeners := stunonly.Listeners{
				Packet: func(network, address string) (net.PacketConn, error) {
					if address == "unavailable" {
						return nil, errors.New("controlled UDP failure")
					}
					conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
					if err == nil {
						packets = append(packets, conn)
					}
					return conn, err
				},
				TCP: func(network, address string) (net.Listener, error) {
					if scenario == "admin-failure" {
						return nil, errors.New("controlled admin failure")
					}
					var err error
					admin, err = net.Listen("tcp4", "127.0.0.1:0")
					return admin, err
				},
			}
			udp := "first,second"
			if scenario == "bind-failure" {
				udp = "unavailable,second"
			}
			if scenario == "disabled" {
				udp = ""
			}
			args := []string{"-listen", "127.0.0.1:0", "-state-dir", t.TempDir(), "-stun-udp", udp}
			if scenario == "invalid-limits" {
				args = append(args, "-stun-requests-per-second", "-1")
			}
			ready := make(chan net.Addr, 1)
			done := make(chan error, 1)
			go func() {
				done <- runWithSTUNListeners(ctx, args, func(addr net.Addr) error { ready <- addr; return nil }, t.Logf, listeners)
			}()
			var relay net.Addr
			select {
			case relay = <-ready:
			case err := <-done:
				t.Fatalf("relay exited before readiness: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("relay readiness timeout")
			}
			// The ready callback synchronizes publication of the process-owned sockets.
			if scenario == "disabled" {
				if len(packets) != 0 || admin != nil {
					t.Fatal("disabled STUN acquired listeners")
				}
			} else if scenario != "invalid-limits" {
				checkSTUNBinding(t, packets[len(packets)-1].LocalAddr())
			}
			if scenario == "runtime-failure" {
				_ = packets[0].Close()
				checkSTUNBinding(t, packets[1].LocalAddr())
			}
			status, _ := readServiceHTTP(t, relay, "/healthz")
			if status != http.StatusOK {
				t.Fatalf("relay health: %d", status)
			}
			// Exercise the application route after the STUN failure, beyond a static liveness check.
			status, _ = readServiceHTTP(t, relay, "/v2/ws")
			if status != http.StatusUpgradeRequired {
				t.Fatalf("relay WebSocket handler: %d", status)
			}
			if admin != nil {
				wantHealth := http.StatusOK
				if scenario == "bind-failure" || scenario == "runtime-failure" || scenario == "invalid-limits" {
					wantHealth = http.StatusServiceUnavailable
				}
				deadline := time.Now().Add(2 * time.Second)
				for {
					status, _ = readServiceHTTP(t, admin.Addr(), "/healthz")
					if status == wantHealth {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("STUN health %d, want %d", status, wantHealth)
					}
					time.Sleep(time.Millisecond)
				}
				_, metrics := readServiceHTTP(t, admin.Addr(), "/metrics")
				if !strings.Contains(metrics, "windshare_stun_healthy") {
					t.Fatal(metrics)
				}
				if scenario == "bind-failure" && !strings.Contains(metrics, "windshare_stun_healthy{listener=\"udp-0\"} 0") {
					t.Fatal(metrics)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owned shutdown timeout")
			}
			for _, conn := range packets {
				if err := conn.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("UDP listener survived shutdown: %v", err)
				}
			}
		})
	}
}

func TestReadinessFailureClosesSTUNSockets(t *testing.T) {
	var packet net.PacketConn
	err := runWithSTUNListeners(context.Background(), []string{"-listen", "127.0.0.1:0", "-state-dir", t.TempDir(), "-stun-udp", "controlled", "-stun-admin", ""},
		func(net.Addr) error { return errors.New("readiness failed") }, t.Logf, stunonly.Listeners{
			Packet: func(string, string) (net.PacketConn, error) {
				var err error
				packet, err = net.ListenPacket("udp4", "127.0.0.1:0")
				return packet, err
			},
		})
	if err == nil || packet == nil {
		t.Fatalf("readiness result: %v", err)
	}
	if err := packet.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("readiness leaked UDP: %v", err)
	}
}

func checkSTUNBinding(t *testing.T, address net.Addr) {
	t.Helper()
	conn, err := net.Dial("udp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err := conn.Write(request.Raw); err != nil {
		t.Fatal(err)
	}
	response := &stun.Message{Raw: make([]byte, stunonly.MaximumDatagramBytes)}
	n, err := conn.Read(response.Raw)
	if err != nil {
		t.Fatal(err)
	}
	response.Raw = response.Raw[:n]
	if err := response.Decode(); err != nil {
		t.Fatal(err)
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(response); err != nil {
		t.Fatal(err)
	}
	local := conn.LocalAddr().(*net.UDPAddr)
	if response.TransactionID != request.TransactionID || !mapped.IP.Equal(local.IP) || mapped.Port != local.Port {
		t.Fatalf("binding response differs from actual source: %+v %v", mapped, local)
	}
}

func readServiceHTTP(t *testing.T, address net.Addr, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address.String() + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}
