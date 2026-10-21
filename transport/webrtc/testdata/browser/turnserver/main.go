package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pion/turn/v5"
)

const (
	controlListenAddress     = "127.0.0.1:0"
	controlAdvertisedAddress = "127.0.0.1"
	routingProbeAddress      = "192.0.2.1:9"
	realm                    = "windshare.test"
	username                 = "windshare-browser"
	credential               = "windshare-local-turn"
	sharedSecret             = "windshare-local-turn-issuer"
)

type readyRecord struct {
	Component    string `json:"component"`
	ScenarioID   string `json:"scenarioId"`
	OperationID  string `json:"operationId"`
	Milestone    string `json:"milestone"`
	URL          string `json:"url"`
	TCPURL       string `json:"tcpUrl"`
	RelayAddress string `json:"relayAddress"`
	Username     string `json:"username"`
	Credential   string `json:"credential"`
	SharedSecret string `json:"sharedSecret"`
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
	// Both protocols share one service address, so the product must select a
	// transport rather than accidentally selecting a different relay endpoint.
	tcp, err := net.Listen("tcp4", listener.LocalAddr().String())
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			if attributes.Username != username {
				expiry, _, found := strings.Cut(attributes.Username, ":")
				seconds, err := strconv.ParseInt(expiry, 10, 64)
				if !found || err != nil || time.Now().Unix() >= seconds {
					return "", nil, false
				}
				mac := hmac.New(sha1.New, []byte(sharedSecret))
				_, _ = mac.Write([]byte(attributes.Username))
				password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
				return attributes.Username, turn.GenerateAuthKey(attributes.Username, realm, password), true
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
		ListenerConfigs: []turn.ListenerConfig{{Listener: tcp, RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
			RelayAddress: relayIP, Address: relayIP.String(),
		}}},
	})
	if err != nil {
		return errors.Join(err, listener.Close(), tcp.Close())
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
		TCPURL:       fmt.Sprintf("turn:%s?transport=tcp", net.JoinHostPort(controlAdvertisedAddress, fmt.Sprint(udp.Port))),
		RelayAddress: relayIPv4.String(),
		Username:     username,
		Credential:   credential,
		SharedSecret: sharedSecret,
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
