package protocolsession

import "testing"

func mustControlBody(t testing.TB, value map[uint64]any) []byte {
	t.Helper()
	body, err := EncodeBody(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
