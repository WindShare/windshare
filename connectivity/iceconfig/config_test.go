package iceconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestImmutableDiscoveryConfiguration(t *testing.T) {
	discovery := []string{"stun:stun.example", "stun:[2001:db8::1]:65535"}
	connection, err := NewDirect(discovery)
	if err != nil {
		t.Fatal(err)
	}
	wantServers := []STUNServer{{Host: "stun.example", Port: 3478}, {Host: "2001:db8::1", Port: 65535}}

	discovery[0] = "changed by caller"
	connection.DiscoveryServers()[0] = STUNServer{}
	if got := connection.DiscoveryServers(); !slices.Equal(got, wantServers) {
		t.Fatalf("discovery servers = %v, want %v", got, wantServers)
	}
}

func TestInvalidDiscoveryEndpoint(t *testing.T) {
	for _, raw := range []string{
		"turn:relay.example",
		"stun:stun.example:0",
		"stun:stun.example:-1",
		"stun:stun.example:69014",
		"stun:" + strings.Repeat("a", MaximumURLBytes),
	} {
		if _, err := NewDirect([]string{raw}); !errors.Is(err, ErrConfiguration) {
			t.Errorf("NewDirect(%q) error = %v, want ErrConfiguration", raw, err)
		}
	}
}

func TestTURNConfigurationOwnsCredentialsAndRejectsDiscoveryURLs(t *testing.T) {
	const endpoint = "turns:[2001:db8::1]:443?transport=tcp"
	urls := []string{endpoint}
	config, err := NewTURNRelay(urls, "user", "password")
	if err != nil {
		t.Fatal(err)
	}
	urls[0] = "changed"
	first, user, password := config.Credentials()
	if first[0] != endpoint || user != "user" || password != "password" {
		t.Fatal("TURN credentials were not preserved")
	}
	first[0] = "changed"
	again, _, _ := config.Credentials()
	if again[0] != endpoint {
		t.Fatal("mutable TURN configuration")
	}
	if _, err := NewTURNRelay([]string{"stun:localhost"}, "user", "password"); err == nil {
		t.Fatal("accepted STUN")
	}
	if _, err := NewTURNRelay(again, "", "password"); err == nil {
		t.Fatal("accepted missing credentials")
	}
}
