package iceconfig

import "testing"

func TestImmutableDiscoveryConfiguration(t *testing.T) {
	discovery := []string{"stun:stun.example:3478"}
	connection, err := NewDirect(discovery)
	if err != nil {
		t.Fatal(err)
	}
	discovery[0] = "bad"
	got := connection.DiscoveryURLs()
	got[0] = "bad"
	if connection.DiscoveryURLs()[0] != "stun:stun.example:3478" {
		t.Fatal("mutable configuration")
	}
	if _, err := NewDirect([]string{"turn:relay.example"}); err == nil {
		t.Fatal("non-STUN URL leaked into discovery")
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
