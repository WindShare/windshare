package iceconfig

import (
	"slices"
	"testing"
)

func TestRelayTransportsPreferTCPAndNarrowBothPeerInputs(t *testing.T) {
	credential, _ := Fixed("user", "password")
	urls := []string{"turn:example.org", "turns:example.org:443", "turn:example.org:3478?transport=tcp", "turn:backup.org?transport=tcp"}
	relay, err := NewRelay(Identity{Provider: "test", Account: "owner", Profile: "primary"}, urls, credential)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(relay.Transports(), []RelayTransport{RelayTCP, RelayTLS, RelayUDP}) {
		t.Fatal("initial protocol preference is not TCP, TLS, UDP")
	}
	tcp, err := relay.WithTransport(RelayTCP)
	if err != nil || !slices.Equal(tcp.URLs(), urls[2:]) || tcp.Identity() != relay.Identity() || tcp.Credentials() != credential {
		t.Fatalf("narrowing changed service authority: %v", err)
	}
	if !slices.Equal(relay.URLs(), urls) {
		t.Fatal("narrowing mutated profile alternatives")
	}
	if _, err := tcp.WithTransport(RelayUDP); err == nil {
		t.Fatal("invented an unconfigured transport")
	}
}
