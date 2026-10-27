package outputcap

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestOutputRootBindingOwnsCanonicalCertifiedIdentity(t *testing.T) {
	identityScheme, err := NewIdentitySchemeID(string(IdentitySchemeLinuxHandleV2))
	if err != nil {
		t.Fatal(err)
	}
	volume := []byte("volume")
	object := []byte("root-object")
	binding, err := NewOutputRootBinding(identityScheme, volume, object)
	if err != nil {
		t.Fatal(err)
	}
	if binding.IsZero() || binding.IdentityScheme() != identityScheme ||
		len(binding.Bytes()) != OutputRootBindingBytes || binding.String() != hex.EncodeToString(binding.Bytes()) {
		t.Fatalf("root binding = %+v", binding)
	}

	volume[0] ^= 0xff
	object[0] ^= 0xff
	returned := binding.Bytes()
	returned[0] ^= 0xff
	if bytes.Equal(returned, binding.Bytes()) {
		t.Fatal("root binding exposed caller-owned byte storage")
	}
	repeated, err := NewOutputRootBinding(
		identityScheme, []byte("volume"), []byte("root-object"),
	)
	if err != nil || repeated != binding {
		t.Fatalf("repeated root binding = %+v, %v", repeated, err)
	}
	different, err := NewOutputRootBinding(
		identityScheme, []byte("volume"), []byte("other-object"),
	)
	if err != nil || different == binding {
		t.Fatalf("different root binding = %+v, %v", different, err)
	}
	if !(OutputRootBinding{}).IsZero() {
		t.Fatal("zero root binding reported live authority")
	}
}

func TestOutputRootBindingRejectsIncompleteOrOversizedClaims(t *testing.T) {
	oversized := []byte(strings.Repeat("x", MaxRootIdentityClaimBytes+1))
	for name, test := range map[string]struct {
		identityScheme IdentitySchemeID
		volume         []byte
		object         []byte
	}{
		"missing identityScheme": {volume: []byte("volume"), object: []byte("object")},
		"missing volume": {
			identityScheme: IdentitySchemeLinuxHandleV2,
			object:         []byte("object"),
		},
		"oversized volume": {
			identityScheme: IdentitySchemeLinuxHandleV2,
			volume:         oversized,
			object:         []byte("object"),
		},
		"missing object": {
			identityScheme: IdentitySchemeLinuxHandleV2,
			volume:         []byte("volume"),
		},
		"oversized object": {
			identityScheme: IdentitySchemeLinuxHandleV2,
			volume:         []byte("volume"),
			object:         oversized,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOutputRootBinding(test.identityScheme, test.volume, test.object); !errors.Is(err, ErrInvalidRootBinding) {
				t.Fatalf("root binding error = %v", err)
			}
		})
	}
	if _, err := NewIdentitySchemeID("future-identityScheme"); err == nil {
		t.Fatal("unknown identityScheme was accepted")
	}
}

func TestDestinationBindingIDCommitsToRootAndControlNamespaceOnly(t *testing.T) {
	if DestinationBindingIDDomainV1 != "windshare/destination-authority-id/v1" {
		t.Fatalf("destination binding domain = %q", DestinationBindingIDDomainV1)
	}
	id, err := NewDestinationBindingID([]byte("root-native-object"), []byte("control-namespace-object"))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := NewDestinationBindingID([]byte("root-native-object"), []byte("control-namespace-object"))
	if err != nil || repeated != id || id.IsZero() ||
		len(id.Bytes()) != DestinationBindingIDBytes ||
		id.String() != hex.EncodeToString(id.Bytes()) {
		t.Fatalf("destination binding identity = %x, %v", repeated.Bytes(), err)
	}
	changedRoot, _ := NewDestinationBindingID([]byte("other-root"), []byte("control-namespace-object"))
	changedNamespace, _ := NewDestinationBindingID([]byte("root-native-object"), []byte("other-namespace"))
	if changedRoot == id || changedNamespace == id {
		t.Fatal("destination binding identity omitted a native object")
	}
	ref, err := id.AuthorityRef()
	if err != nil || !bytes.Equal(ref.Bytes(), id.Bytes()) {
		t.Fatalf("canonical authority ref = %x, %v", ref.Bytes(), err)
	}
	returned := id.Bytes()
	returned[0] ^= 0xff
	if bytes.Equal(returned, id.Bytes()) {
		t.Fatal("destination binding identity exposed mutable storage")
	}
}

func TestDestinationBindingIDRejectsMissingOversizedAndZeroClaims(t *testing.T) {
	oversizedRoot := bytes.Repeat([]byte{'x'}, MaxRootIdentityClaimBytes+1)
	oversizedNamespace := bytes.Repeat([]byte{'x'}, MaxNamespaceIdentityClaimBytes+1)
	for name, claims := range map[string][2][]byte{
		"missing root":        {nil, []byte("namespace")},
		"missing namespace":   {[]byte("root"), nil},
		"oversized root":      {oversizedRoot, []byte("namespace")},
		"oversized namespace": {[]byte("root"), oversizedNamespace},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDestinationBindingID(claims[0], claims[1]); !errors.Is(err, ErrInvalidDestinationBindingID) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := DestinationBindingIDFromBytes(make([]byte, DestinationBindingIDBytes)); !errors.Is(err, ErrInvalidDestinationBindingID) {
		t.Fatalf("zero identity error = %v", err)
	}
	if _, err := (DestinationBindingID{}).AuthorityRef(); !errors.Is(err, ErrInvalidDestinationBindingID) {
		t.Fatalf("zero authority ref error = %v", err)
	}
}
