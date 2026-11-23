package iceconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestImmutableDiscoveryConfiguration(t *testing.T) {
	discovery := []string{"stun:stun.example", "stun:[2001:db8::1]:65535"}
	connection, err := New(discovery)
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
		if _, err := New([]string{raw}); !errors.Is(err, ErrConfiguration) {
			t.Errorf("New(%q) error = %v, want ErrConfiguration", raw, err)
		}
	}
}
