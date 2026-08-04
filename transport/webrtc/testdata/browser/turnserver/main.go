package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/pion/turn/v5"
)

const (
	controlListenAddress     = "127.0.0.1:0"
	controlAdvertisedAddress = "127.0.0.1"
	routingProbeAddress      = "192.0.2.1:9"
	realm                    = "windshare.test"
	username                 = "windshare-browser"
	credential               = "windshare-local-turn"
)

type readyRecord struct {
	Component    string `json:"component"`
	ScenarioID   string `json:"scenarioId"`
	OperationID  string `json:"operationId"`
	Milestone    string `json:"milestone"`
	URL          string `json:"url"`
	RelayAddress string `json:"relayAddress"`
	Username     string `json:"username"`
	Credential   string `json:"credential"`
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	relayIP, err := routedRelayIPv4()
	if err != nil {
		return err
	}
	listener, err := net.ListenPacket("udp4", controlListenAddress)
	if err != nil {
		return fmt.Errorf("listen for local TURN: %w", err)
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			if attributes.Username != username {
				return "", nil, false
			}
			return username, turn.GenerateAuthKey(username, realm, credential), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: listener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: relayIP,
				Address:      relayIP.String(),
			},
		}},
	})
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	defer func() {
		runErr = errors.Join(runErr, server.Close())
	}()
	record, err := newReadyRecord(listener.LocalAddr(), relayIP)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
		return fmt.Errorf("publish local TURN readiness: %w", err)
	}

	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopping)
	<-stopping
	return nil
}

func newReadyRecord(address net.Addr, relayIP net.IP) (readyRecord, error) {
	udp, ok := address.(*net.UDPAddr)
	if !ok || udp == nil || !udp.IP.Equal(net.IPv4(127, 0, 0, 1)) || udp.Port < 1 || udp.Port > 65_535 {
		return readyRecord{}, fmt.Errorf("local TURN listener did not publish an IPv4 loopback address")
	}
	relayIPv4 := relayIP.To4()
	if relayIPv4 == nil || relayIPv4.IsLoopback() || relayIPv4.IsUnspecified() || relayIPv4.IsMulticast() {
		return readyRecord{}, fmt.Errorf("local TURN relay did not publish an owned non-loopback IPv4 address")
	}
	return readyRecord{
		Component:    "browser-local-turn-server",
		ScenarioID:   "chromium-turn-route",
		OperationID:  "chromium-turn-route-server",
		Milestone:    "listener-ready",
		URL:          fmt.Sprintf("turn:%s?transport=udp", net.JoinHostPort(controlAdvertisedAddress, fmt.Sprint(udp.Port))),
		RelayAddress: relayIPv4.String(),
		Username:     username,
		Credential:   credential,
	}, nil
}

func routedRelayIPv4() (net.IP, error) {
	// Pion deliberately omits loopback host candidates in the product sender. A
	// loopback allocation therefore gathers successfully but can never prove the
	// TURN route, while a wildcard allocation is observed as peer-reflexive. A
	// connected UDP socket selects the same owned route without sending traffic.
	target, err := net.ResolveUDPAddr("udp4", routingProbeAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve local TURN routing probe: %w", err)
	}
	connection, err := net.DialUDP("udp4", nil, target)
	if err != nil {
		return nil, fmt.Errorf("resolve local TURN route: %w", err)
	}
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address == nil {
		return nil, errors.Join(errors.New("local TURN route did not select a UDP address"), connection.Close())
	}
	relayIP := append(net.IP(nil), address.IP.To4()...)
	if len(relayIP) == 0 || relayIP.IsLoopback() || relayIP.IsUnspecified() || relayIP.IsMulticast() {
		return nil, errors.Join(errors.New("local TURN route did not select a usable non-loopback IPv4 address"), connection.Close())
	}
	if err := connection.Close(); err != nil {
		return nil, fmt.Errorf("close local TURN routing probe: %w", err)
	}
	return relayIP, nil
}
