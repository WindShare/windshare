package v2

import (
	"bytes"
	"testing"
)

func TestSessionControlValidation(t *testing.T) {
	id := RelaySessionID{1}
	credit := SessionCredit{RelaySessionID: id, Frames: 2, Bytes: 100}
	admitted := SessionAdmitted{RelaySessionID: id}
	encoded, err := credit.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseSessionCredit(encoded)
	if err != nil || decoded != credit {
		t.Fatal(decoded, err)
	}
	confirmation, err := admitted.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSessionAdmitted(confirmation)
	if err != nil || parsed != admitted {
		t.Fatal(parsed, err)
	}
	for _, sample := range []struct {
		wire  []byte
		parse func([]byte) error
	}{
		{encoded, func(b []byte) error { _, err := ParseSessionCredit(b); return err }},
		{confirmation, func(b []byte) error { _, err := ParseSessionAdmitted(b); return err }},
	} {
		for length := 0; length < len(sample.wire); length++ {
			if sample.parse(sample.wire[:length]) == nil {
				t.Fatal("accepted truncation", length)
			}
		}
		if sample.parse(append(bytes.Clone(sample.wire), 0)) == nil {
			t.Fatal("accepted trailing byte")
		}
		for _, offset := range []int{0, 4, 5, 6, 7} {
			bad := bytes.Clone(sample.wire)
			bad[offset] ^= 1
			if sample.parse(bad) == nil {
				t.Fatal("accepted malformed prefix", offset)
			}
		}
		bad := bytes.Clone(sample.wire)
		clear(bad[8:16])
		if sample.parse(bad) == nil {
			t.Fatal("accepted zero session")
		}
	}
	for _, invalid := range []SessionCredit{
		{}, {RelaySessionID: id}, {RelaySessionID: id, Frames: 65, Bytes: 2000},
		{RelaySessionID: id, Frames: 1, Bytes: 1}, {RelaySessionID: id, Frames: 1, Bytes: SenderWindowBytes + 1},
		{RelaySessionID: id, Frames: 1, Bytes: MaxOpaqueCiphertextBytes + OpaqueRouteHeaderBytes + 1},
	} {
		if _, err := invalid.MarshalBinary(); err == nil {
			t.Fatal("accepted impossible credit", invalid)
		}
	}
	bad := bytes.Clone(encoded)
	clear(bad[16:20])
	if _, err := ParseSessionCredit(bad); err == nil {
		t.Fatal("accepted zero frame credit")
	}
	if _, err := (SessionAdmitted{}).MarshalBinary(); err == nil {
		t.Fatal("accepted zero admission identity")
	}
}
