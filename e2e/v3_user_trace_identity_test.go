package e2e

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func v3TraceIdentityFromHex(t *testing.T, value string) string {
	t.Helper()
	identity, err := hex.DecodeString(value)
	if err != nil || len(identity) != v3IdentityBytes {
		t.Fatalf("invalid hexadecimal v3 trace identity %q: bytes=%d err=%v", value, len(identity), err)
	}
	return base64.RawURLEncoding.EncodeToString(identity)
}
