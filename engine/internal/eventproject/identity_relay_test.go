package eventproject

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestDomainIdentityBridgesPreserveFullWidthsAndSemanticTypes(t *testing.T) {
	raw := identityFixture(0x30)
	receive, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	session, err := protocolsession.ProtocolSessionIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.TransferJobIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		got  func() ([]byte, string, error)
	}{
		{"receive", func() ([]byte, string, error) {
			id, err := ReceiveOperationID(receive)
			return id.Bytes(), id.Hex(), err
		}},
		{"session", func() ([]byte, string, error) {
			id, err := ProtocolSessionID(session)
			return id.Bytes(), id.Hex(), err
		}},
		{"transfer job", func() ([]byte, string, error) {
			id, err := TransferJobID(job)
			return id.Bytes(), id.Hex(), err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, encoded, err := test.got()
			if err != nil || !bytes.Equal(got, raw) || encoded != fmt.Sprintf("%x", raw) || len(encoded) != len(raw)*2 {
				t.Fatalf("bridge = %x/%q err=%v", got, encoded, err)
			}
		})
	}
	if _, err := ReceiveOperationID(receivecontract.OperationID{}); err == nil {
		t.Fatal("accepted zero receive operation")
	}
	lane, err := LaneIdentity(sessioncontract.LaneIdentity{ID: 4, Epoch: 0})
	if err != nil || lane.ID() != 4 || lane.Epoch() != 0 {
		t.Fatalf("lane bridge = %+v err=%v", lane, err)
	}
}

func TestRelayOriginProjectionRetainsOnlyCanonicalOrigin(t *testing.T) {
	const token = "QUERY-TOKEN-CANARY"
	const path = "provider/private/base"
	raw := "HTTPS://Relay.Example:443/" + path + "?auth=" + token
	origin, err := NormalizeRelayOrigin(raw)
	if err != nil {
		t.Fatal(err)
	}
	scheme, ok := origin.Scheme().Name()
	if !ok || scheme != "wss" || origin.Host() != "relay.example" || origin.Port() != 443 {
		t.Fatalf("origin = scheme=%q host=%q port=%d", scheme, origin.Host(), origin.Port())
	}
	projection := fmt.Sprintf("%#v", origin)
	for _, forbidden := range []string{token, path, "auth=", "/v2/ws"} {
		if strings.Contains(projection, forbidden) {
			t.Fatalf("origin retained forbidden relay URL material %q: %s", forbidden, projection)
		}
	}

	endpoint, err := v2.NormalizeRelayEndpoint("ws://[2001:db8::1]:8080/base?x=secret")
	if err != nil {
		t.Fatal(err)
	}
	ipv6, err := RelayOrigin(endpoint)
	if err != nil || ipv6.Host() != "2001:db8::1" || ipv6.Port() != 8080 {
		t.Fatalf("IPv6 origin = %+v err=%v", ipv6, err)
	}
	endpoint.IdentityURL += "/mutated"
	if _, err := RelayOrigin(endpoint); err == nil {
		t.Fatal("accepted caller-mutated normalized endpoint")
	}
}

func identityFixture(seed byte) []byte {
	value := make([]byte, 16)
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}
