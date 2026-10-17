package v2

import (
	"bytes"
	"errors"
	"testing"
)

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
