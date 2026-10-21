package turncontrol

import (
	"bytes"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func TestDelegationCanonicalAuthorityAndLifetime(t *testing.T) {
	header := Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 3}
	grant := Grant{Authority: header, PathID: [16]byte{4}, Replaces: [16]byte{5}, Provider: "coturn", Account: "home", Profile: "primary",
		CredentialKind: Expiring, Username: "123:user", Password: "password", ValidFor: time.Hour, URLs: []string{"turn:127.0.0.1:3478?transport=udp", "turns:example.org:443?transport=tcp"}}
	messages := []Message{Demand{header, MaximumDemandLifetime}, grant, Ready{header, grant.PathID},
		Retire{header, grant.PathID, Replaced}, Unavailable{header, "primary", time.Second, "provider unavailable"},
		Available{header, []string{"primary", "secondary"}}, Available{header, []string{}}, Report{header, grant.PathID, 1 << 40},
		Recover{header, grant.PathID, AllocationStateMismatch}, Recover{header, grant.PathID, ChannelUnavailable}}
	for _, message := range messages {
		body, err := Encode(message)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(body)
		if err != nil || got.Header() != header {
			t.Fatalf("decode %T: %v", message, err)
		}
		reencoded, _ := Encode(got)
		if !bytes.Equal(body, reencoded) {
			t.Fatal("canonical encoding changed")
		}
		if _, err := Decode(append(body, 0)); err == nil {
			t.Fatal("accepted trailing data")
		}
		var fields []any
		if err := cbor.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		fields[2] = make([]byte, 15)
		invalid, _ := cbor.Marshal(fields)
		if _, err := Decode(invalid); err == nil {
			t.Fatal("accepted truncated authority")
		}
	}
	grant.CredentialKind = Fixed
	if _, err := Encode(grant); err == nil {
		t.Fatal("fixed credential with expiry")
	}
	grant.ValidFor = 0
	if _, err := Encode(grant); err != nil {
		t.Fatal(err)
	}
	grant.Replaces = grant.PathID
	if _, err := Encode(grant); err == nil {
		t.Fatal("path replaced itself")
	}
	if _, err := Encode(Demand{header, MaximumDemandLifetime + time.Millisecond}); err == nil {
		t.Fatal("unbounded demand")
	}
	for _, reason := range []RecoveryReason{0, ChannelUnavailable + 1} {
		if _, err := Encode(Recover{header, grant.PathID, reason}); err == nil {
			t.Fatal("invalid recovery reason")
		}
	}
}

func TestRejectNoncanonicalAndZeroAuthority(t *testing.T) {
	header := Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}
	body, _ := Encode(Demand{header, 0})
	noncanonical := append([]byte{body[0], 0x18, byte(Schema)}, body[2:]...)
	for _, invalid := range [][]byte{nil, noncanonical, bytes.Repeat([]byte{1}, MaximumBodyBytes+1), {0x86, 3, 1}} {
		if _, err := Decode(invalid); err == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
	if _, err := Encode(Demand{}); err == nil {
		t.Fatal("accepted zero authority")
	}
}
