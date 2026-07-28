package resumestate

import "testing"

func TestRecordUpdateTemporaryPrefixValidatesCanonicalTargets(t *testing.T) {
	for _, target := range []string{HeaderRecordName, ControlRecordName, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.state"} {
		prefix, err := RecordUpdateTemporaryPrefix(target)
		if err != nil || prefix == "" {
			t.Fatalf("prefix %q = %q, %v", target, prefix, err)
		}
	}
	for _, target := range []string{"", "header", "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789.state", "../escape.state"} {
		if prefix, err := RecordUpdateTemporaryPrefix(target); err == nil || prefix != "" {
			t.Fatalf("invalid target %q returned prefix %q, err %v", target, prefix, err)
		}
	}
}
