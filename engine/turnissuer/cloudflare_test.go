package turnissuer

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	p "github.com/windshare/windshare/core/session/protocolsession"
)

func TestCloudflareCombinesOnlyUsableURLsForOneCredential(t *testing.T) {
	servers := []map[string]any{
		{"urls": "stun:discover.example"},
		{"urls": "turn:ignored.example", "username": "filtered", "credential": "unused"},
		{"urls": "turn:office.example", "username": "invalid", "credential": strings.Repeat("x", p.MaxTURNCredentialBytes+1)},
		{"urls": "turn:office.example?transport=udp", "username": "user", "credential": "password"},
		{"urls": "turn:office.example?transport=tcp", "username": "another", "credential": "password"},
		{"urls": "turn:office.example?transport=tcp", "username": "user", "credential": "another-password"},
		{"urls": []string{"turn:office.example?transport=udp", "turn:bad@example.org", "turns:office.example:443"}, "username": "user", "credential": "password"},
	}
	response, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectCloudflareServers(response, []string{"turn:office.example", "turns:office.example:443"})
	if err != nil || selected.Username != "user" || selected.Credential != "password" ||
		!slices.Equal(selected.URLs, []string{"turn:office.example?transport=udp", "turns:office.example:443"}) {
		t.Fatalf("incorrect credentials or transport selection: %v, urls %v", err, selected.URLs)
	}
	if _, err := selectCloudflareServers(response, []string{"turn:other.example"}); !errors.Is(err, errUnavailable) {
		t.Fatal("no matching URL should leave the service unavailable", err)
	}
}

func TestCloudflareSkipsGroupsRejectedByGrantWireFormat(t *testing.T) {
	const acceptedURL = "turns:relay.example:443"
	const acceptedUser = "caf\u00e9"
	streamSafe := "x" + strings.Repeat("\u0334", 30)
	if !p.ValidTURNCredential(streamSafe) {
		t.Fatal("grant rejected a stream-safe credential")
	}
	for _, first := range []struct{ url, username, password string }{
		{"turn:old.example", "cafe\u0301", "bad"},
		{"turn:old.example", "first", "cafe\u0301"},
		{"turn:old.example", streamSafe + "\u0334", "bad"},
		{"turn:bad..example", "first", "bad"},
		{"turn:-bad.example", "first", "bad"},
		{"turn:ab--cd.example", "first", "bad"},
		{"turn:127.1", "first", "bad"},
		{"turn:" + strings.Repeat("x", 64) + ".example", "first", "bad"},
	} {
		servers := []map[string]string{
			{"urls": first.url, "username": first.username, "credential": first.password},
			{"urls": acceptedURL, "username": acceptedUser, "credential": "valid"},
		}
		response, err := json.Marshal(servers)
		if err != nil {
			t.Fatal(err)
		}
		selected, err := selectCloudflareServers(response, nil)
		if err != nil || selected.Username != acceptedUser || !slices.Equal(selected.URLs, []string{acceptedURL}) {
			t.Fatalf("first authority %q displaced usable credential: %v", first.url, err)
		}
	}
}

func TestCloudflareSingleServerLimitsDistinctURLs(t *testing.T) {
	var urls []string
	for i := range p.MaxURLsPerGrant + 1 {
		urls = append(urls, fmt.Sprintf("turn:relay-%d.example", i))
	}
	response, err := json.Marshal(map[string]any{"urls": append([]string{urls[0]}, urls...), "username": "user", "credential": "password"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectCloudflareServers(response, nil)
	if err != nil || !slices.Equal(selected.URLs, urls[:p.MaxURLsPerGrant]) {
		t.Fatalf("unique transport limit: %v, urls %v", err, selected.URLs)
	}
}
