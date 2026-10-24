package outputcap

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestOutputRootBindingOwnsCanonicalCertifiedIdentity(t *testing.T) {
	certification, err := NewCertificationID(string(CertificationLinuxExt4ProcessRestart))
	if err != nil {
		t.Fatal(err)
	}
	volume := []byte("volume")
	object := []byte("root-object")
	binding, err := NewOutputRootBinding(certification, volume, object)
	if err != nil {
		t.Fatal(err)
	}
	if binding.IsZero() || binding.Certification() != certification ||
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
		certification, []byte("volume"), []byte("root-object"),
	)
	if err != nil || repeated != binding {
		t.Fatalf("repeated root binding = %+v, %v", repeated, err)
	}
	different, err := NewOutputRootBinding(
		certification, []byte("volume"), []byte("other-object"),
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
		certification CertificationID
		volume        []byte
		object        []byte
	}{
		"missing certification": {volume: []byte("volume"), object: []byte("object")},
		"missing volume": {
			certification: CertificationLinuxExt4ProcessRestart,
			object:        []byte("object"),
		},
		"oversized volume": {
			certification: CertificationLinuxExt4ProcessRestart,
			volume:        oversized,
			object:        []byte("object"),
		},
		"missing object": {
			certification: CertificationLinuxExt4ProcessRestart,
			volume:        []byte("volume"),
		},
		"oversized object": {
			certification: CertificationLinuxExt4ProcessRestart,
			volume:        []byte("volume"),
			object:        oversized,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOutputRootBinding(test.certification, test.volume, test.object); !errors.Is(err, ErrInvalidRootBinding) {
				t.Fatalf("root binding error = %v", err)
			}
		})
	}
	if _, err := NewCertificationID("future-certification"); err == nil {
		t.Fatal("unknown certification was accepted")
	}
}

func TestDestinationAuthorityIDCommitsToRootAndControlNamespaceOnly(t *testing.T) {
	if DestinationAuthorityIDDomainV1 != "windshare/destination-authority-id/v1" {
		t.Fatalf("destination authority domain = %q", DestinationAuthorityIDDomainV1)
	}
	id, err := NewDestinationAuthorityID([]byte("root-native-object"), []byte("control-namespace-object"))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := NewDestinationAuthorityID([]byte("root-native-object"), []byte("control-namespace-object"))
	if err != nil || repeated != id || id.IsZero() ||
		len(id.Bytes()) != DestinationAuthorityIDBytes ||
		id.String() != hex.EncodeToString(id.Bytes()) {
		t.Fatalf("destination authority identity = %x, %v", repeated.Bytes(), err)
	}
	changedRoot, _ := NewDestinationAuthorityID([]byte("other-root"), []byte("control-namespace-object"))
	changedNamespace, _ := NewDestinationAuthorityID([]byte("root-native-object"), []byte("other-namespace"))
	if changedRoot == id || changedNamespace == id {
		t.Fatal("destination authority identity omitted a native object")
	}
	ref, err := id.AuthorityRef()
	if err != nil || !bytes.Equal(ref.Bytes(), id.Bytes()) {
		t.Fatalf("canonical authority ref = %x, %v", ref.Bytes(), err)
	}
	returned := id.Bytes()
	returned[0] ^= 0xff
	if bytes.Equal(returned, id.Bytes()) {
		t.Fatal("destination authority identity exposed mutable storage")
	}
}

func TestDestinationAuthorityIDRejectsMissingOversizedAndZeroClaims(t *testing.T) {
	oversizedRoot := bytes.Repeat([]byte{'x'}, MaxRootIdentityClaimBytes+1)
	oversizedNamespace := bytes.Repeat([]byte{'x'}, MaxNamespaceIdentityClaimBytes+1)
	for name, claims := range map[string][2][]byte{
		"missing root":        {nil, []byte("namespace")},
		"missing namespace":   {[]byte("root"), nil},
		"oversized root":      {oversizedRoot, []byte("namespace")},
		"oversized namespace": {[]byte("root"), oversizedNamespace},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDestinationAuthorityID(claims[0], claims[1]); !errors.Is(err, ErrInvalidDestinationAuthorityID) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := DestinationAuthorityIDFromBytes(make([]byte, DestinationAuthorityIDBytes)); !errors.Is(err, ErrInvalidDestinationAuthorityID) {
		t.Fatalf("zero identity error = %v", err)
	}
	if _, err := (DestinationAuthorityID{}).AuthorityRef(); !errors.Is(err, ErrInvalidDestinationAuthorityID) {
		t.Fatalf("zero authority ref error = %v", err)
	}
}
