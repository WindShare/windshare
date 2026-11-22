package iceconfig

import "testing"

func TestImmutableDiscoveryConfiguration(t *testing.T) {
	discovery := []string{"stun:stun.example:3478"}
	connection, err := New(discovery)
	if err != nil {
		t.Fatal(err)
	}
	discovery[0] = "bad"
	got := connection.DiscoveryURLs()
	got[0] = "bad"
	if connection.DiscoveryURLs()[0] != "stun:stun.example:3478" {
		t.Fatal("mutable configuration")
	}
	if _, err := New([]string{"turn:relay.example"}); err == nil {
		t.Fatal("non-STUN URL leaked into discovery")
	}
}
