package iceconfig

import (
	"testing"
	"time"
)

func TestImmutableAuthorityAndSeparateDiscovery(t *testing.T) {
	now := time.Unix(1000, 0)
	credentials, err := Expiring("user", "password", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	urls := []string{"turn:127.0.0.1:3478?transport=udp", "turns:turn.example:443?transport=tcp"}
	relay, err := NewRelay(Identity{"coturn", "account", "profile"}, urls, credentials)
	if err != nil {
		t.Fatal(err)
	}
	discovery := []string{"stun:stun.example:3478"}
	connection, err := New(discovery, &relay, RelayCandidates)
	if err != nil {
		t.Fatal(err)
	}
	urls[0] = "bad"
	discovery[0] = "bad"
	got, _ := connection.Relay()
	gotURLs := got.URLs()
	gotURLs[0] = "bad"
	if got.URLs()[0] != "turn:127.0.0.1:3478?transport=udp" || connection.DiscoveryURLs()[0] != "stun:stun.example:3478" {
		t.Fatal("mutable configuration")
	}
	if !credentials.ValidAt(now) || credentials.ValidAt(now.Add(time.Minute)) {
		t.Fatal("expiry boundary")
	}
	fixed, _ := Fixed("u", "p")
	if !fixed.ValidAt(now.Add(100*365*24*time.Hour)) || (Credentials{}).ValidAt(now) {
		t.Fatal("implicit credential lifetime")
	}
	if _, err := New([]string{"turn:relay.example"}, nil, AllCandidates); err == nil {
		t.Fatal("relay leaked into discovery")
	}
	if _, err := New(nil, nil, RelayCandidates); err == nil {
		t.Fatal("relay policy without credentials")
	}
}
func TestVerifiedRelayEndpoints(t *testing.T) {
	for _, raw := range []string{"turn:host:3478?transport=udp", "turn:127.0.0.1:3478?transport=tcp", "turns:host:443"} {
		if err := ValidateRelayURL(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"stun:host", "turn:[::1]:3478", "turns:host:443?transport=udp", "turn:user@host", "turn:host:65536", "https://host", "turn:host/secret"} {
		if ValidateRelayURL(raw) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
