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
