package resumestate

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// These small assertions keep the fixed-width identity helpers exercised as
// independent contracts; callers must receive copies, because a mutable slice
// must never become authority for a later resume decision.
func TestFixedIdentityViewsAndTemporaryPrefixes(t *testing.T) {
	object := identity32[OutputObjectID](0x11)
	locator := identity32[LocatorDigest](0x22)
	bootstrap := identity32[BootstrapNonce](0x33)
	for name, got := range map[string][]byte{
		"object":    object.Bytes(),
		"locator":   locator.Bytes(),
		"bootstrap": bootstrap.Bytes(),
	} {
		if len(got) != OutputObjectIDBytes || !bytes.Equal(got, bytes.Repeat([]byte{got[0]}, len(got))) {
			t.Fatalf("%s bytes = %x", name, got)
		}
		got[0]++
	}
	if object[0] != 0x11 || locator[0] != 0x22 || bootstrap[0] != 0x33 {
		t.Fatal("fixed identity Bytes exposed mutable storage")
	}
	if object.String() != strings.Repeat("11", OutputObjectIDBytes) ||
		locator.String() != strings.Repeat("22", OutputObjectIDBytes) ||
		bootstrap.String() != strings.Repeat("33", OutputObjectIDBytes) {
		t.Fatal("fixed identity string encoding changed")
	}

	nonce := identity32[UpdateNonce](0x44)
	for _, target := range []string{HeaderRecordName, ControlRecordName, FileRecordName(locator).Name()} {
		prefix, err := RecordUpdateTemporaryPrefix(target)
		if err != nil || !strings.HasPrefix(target+recordTemporarySeparator+nonce.String(), prefix) {
			t.Fatalf("temporary prefix %q = %q, %v", target, prefix, err)
		}
	}
	for _, target := range []string{"", "header", "not-a-state-name"} {
		if _, err := RecordUpdateTemporaryPrefix(target); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid temporary prefix target %q error = %v", target, err)
		}
	}
}
