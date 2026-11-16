package networkendpoint

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestProtocolEndpointVectorsRemainStableAfterExtraction(t *testing.T) {
	data, err := os.ReadFile("../testvectors/v2-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			Name  string `json:"name"`
			Cases []struct {
				Name     string `json:"name"`
				Raw      string `json:"relayBaseUrl"`
				Accepted bool   `json:"accepted"`
				Dial     string `json:"dialEndpoint"`
				Identity string `json:"relayIdentityEndpoint"`
				Hash     string `json:"relayIdentityB64"`
			} `json:"cases"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, group := range file.Cases {
		if group.Name != "v2-relay-endpoint-normalization" {
			continue
		}
		for _, item := range group.Cases {
			t.Run(item.Name, func(t *testing.T) {
				got, err := NormalizeRelay(item.Raw)
				if !item.Accepted {
					if err == nil {
						t.Fatal("invalid endpoint accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.DialURL != item.Dial || got.IdentityURL != item.Identity || base64.StdEncoding.EncodeToString(got.Identity[:]) != item.Hash {
					t.Fatal(got)
				}
			})
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no endpoint vectors found")
	}
}

func TestSTUNAuthorityCannotAcquireTransportOrCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"stun:STUN.Example:03478": "stun:stun.example:3478", "stun:192.0.2.1:3478": "stun:192.0.2.1:3478",
		"stun:[2001:db8:0:0::1]:3478": "stun:[2001:db8::1]:3478", "stun:[::ffff:192.0.2.1]:3478": "stun:192.0.2.1:3478",
	} {
		got, err := NormalizeSTUN(raw)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "turn:host:3478", "stuns:host:5349", "stun:host", "stun://host:3478", "stun:user@host:3478",
		"stun:host:0", "stun:host:65536", "stun:host:1?transport=tcp", "stun:host:1#secret", "stun:host_name:3478",
		"stun:-host:3478", "stun:127.1:3478", "stun:2130706433:3478", "stun:[bad]:3478", "stun:host:abc", "stun:host\\other:3478"} {
		if _, err := NormalizeSTUN(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

// WHATWG URL parsers (the browser receiver and the Rust relay) treat a host
// whose last label is a decimal or 0x-hex number as IPv4. Go must not send
// such a name to DNS under an identity no other runtime can produce.
func TestHostsEndingInNumberAreNotDNSNames(t *testing.T) {
	for _, host := range []string{"0x7f.0.0.1", "relay.123", "relay.0x10", "relay.0X1f", "relay.0x", "relay.123.",
		"0x7f000001", "0x", "0X", "0xcafe", "relay.09", "127.1"} {
		_, relayErr := NormalizeRelay("https://" + host + ":8443")
		_, stunErr := NormalizeSTUN("stun:" + host + ":3478")
		for _, err := range []error{relayErr, stunErr} {
			failure, ok := errors.AsType[*NumericHostError](err)
			if !errors.Is(err, ErrInvalid) || !ok || failure.Host != strings.ToLower(host) {
				t.Errorf("%s lost its numeric-host cause: %v", host, err)
			}
		}
	}
	for _, host := range []string{"relay.example", "relay123", "123.example", "relay.0x1g", "relay.x10",
		"0xg", "relay.1e3", "relay.0x0x1", "192.0.2.1", "[::1]"} {
		if _, err := NormalizeRelay("https://" + host); err != nil {
			t.Errorf("relay %s rejected: %v", host, err)
		}
		if _, err := NormalizeSTUN("stun:" + host + ":3478"); err != nil {
			t.Errorf("STUN %s rejected: %v", host, err)
		}
	}
}
