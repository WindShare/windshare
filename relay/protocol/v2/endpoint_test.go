package v2

import (
	"bytes"
	"errors"
	"testing"
)

func TestRelayEndpointWhitespaceCauseMatchesRejectedSpelling(t *testing.T) {
	for _, test := range []struct {
		name       string
		address    string
		whitespace bool
	}{
		{name: "leading space", address: " https://relay.example", whitespace: true},
		{name: "trailing newline", address: "https://relay.example\n", whitespace: true},
		{name: "BOM prefix", address: "\ufeffhttps://relay.example", whitespace: true},
		{name: "interior tab", address: "https://relay.example/a\tb", whitespace: true},
		{name: "invalid port with path space", address: "https://relay.example:65536/a b"},
		{name: "invalid scheme with path spacing", address: "ftp://relay.example/a\u00a0b"},
		{name: "control byte with path space", address: "https://relay.example/a b\x1b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NormalizeRelayEndpoint(test.address)
			if !errors.Is(err, ErrIdentity) || errors.Is(err, ErrRelayEndpointWhitespace) != test.whitespace {
				t.Fatalf("NormalizeRelayEndpoint(%q) = %v; whitespace cause = %t", test.address, err, test.whitespace)
			}
		})
	}
	for _, address := range []string{
		"https://relay.example/a b",
		"https://relay.example/a\u00a0b",
		"https://relay.example/a%20b",
	} {
		if _, err := NormalizeRelayEndpoint(address); err != nil {
			t.Fatalf("valid path spelling %q rejected: %v", address, err)
		}
	}
}

func TestRelayEndpointNormalizationVectors(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name                  string `json:"name"`
			RelayBaseURL          string `json:"relayBaseUrl"`
			Accepted              bool   `json:"accepted"`
			DialEndpoint          string `json:"dialEndpoint"`
			RelayIdentityEndpoint string `json:"relayIdentityEndpoint"`
			RelayIdentityB64      string `json:"relayIdentityB64"`
		} `json:"cases"`
	}
	loadV2VectorCase(t, "v2-session.json", "v2-relay-endpoint-normalization", &vectors)
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			got, err := NormalizeRelayEndpoint(vector.RelayBaseURL)
			if !vector.Accepted {
				if !errors.Is(err, ErrIdentity) {
					t.Fatalf("rejected endpoint error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.DialURL != vector.DialEndpoint || got.IdentityURL != vector.RelayIdentityEndpoint ||
				!bytes.Equal(got.Identity[:], testB64(t, vector.RelayIdentityB64)) {
				t.Fatalf("endpoint = %+v", got)
			}
		})
	}
}

func TestRelayShareRoutingVectors(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name                  string `json:"name"`
			RelayBaseURL          string `json:"relayBaseUrl"`
			ShareIDRawB64         string `json:"shareIdRawB64"`
			Accepted              bool   `json:"accepted"`
			DialEndpoint          string `json:"dialEndpoint"`
			RelayIdentityEndpoint string `json:"relayIdentityEndpoint"`
			RelayIdentityB64      string `json:"relayIdentityB64"`
		} `json:"cases"`
	}
	loadV2VectorCase(t, "v2-session.json", "v2-relay-share-routing", &vectors)
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			shareID, err := ShareIDFromBytes(testB64(t, vector.ShareIDRawB64))
			var endpoint RelayEndpoint
			if err == nil {
				endpoint, err = RelayEndpointForShare(vector.RelayBaseURL, shareID)
			}
			if !vector.Accepted {
				if !errors.Is(err, ErrIdentity) {
					t.Fatalf("rejected route error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.DialURL != vector.DialEndpoint || endpoint.IdentityURL != vector.RelayIdentityEndpoint ||
				!bytes.Equal(endpoint.Identity[:], testB64(t, vector.RelayIdentityB64)) {
				t.Fatalf("share endpoint = %+v", endpoint)
			}
			base, err := NormalizeRelayEndpoint(vector.RelayBaseURL)
			if err != nil || base.Identity != endpoint.Identity || base.IdentityURL != endpoint.IdentityURL {
				t.Fatalf("share route changed relay identity: base=%+v error=%v", base, err)
			}
		})
	}
}
