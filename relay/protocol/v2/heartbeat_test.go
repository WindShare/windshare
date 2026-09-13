package v2

import (
	"bytes"
	"testing"
)

func TestConnectionProbeContract(t *testing.T) {
	for _, acknowledgement := range []bool{false, true} {
		nonce := uint64(0x0102030405060708)
		want := []byte{'W', 'S', '2', 'H', 2, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
		marshal := func(n uint64) ([]byte, error) { return (ConnectionProbe{Nonce: n}).MarshalBinary() }
		parse := func(b []byte) (uint64, error) { f, e := ParseConnectionProbe(b); return f.Nonce, e }
		if acknowledgement {
			want[3] = 'A'
			marshal = func(n uint64) ([]byte, error) { return (ConnectionProbeAck{Nonce: n}).MarshalBinary() }
			parse = func(b []byte) (uint64, error) { f, e := ParseConnectionProbeAck(b); return f.Nonce, e }
		}
		encoded, err := marshal(nonce)
		if err != nil || !bytes.Equal(encoded, want) {
			t.Fatalf("encoded=%x error=%v", encoded, err)
		}
		if parsed, err := parse(encoded); err != nil || parsed != nonce {
			t.Fatalf("parsed=%x error=%v", parsed, err)
		}
		if _, err := marshal(0); err == nil {
			t.Fatal("accepted zero nonce")
		}
		for _, offset := range []int{0, 4, 5, 6, 7} {
			bad := bytes.Clone(encoded)
			bad[offset] ^= 1
			if _, err := parse(bad); err == nil {
				t.Fatalf("accepted offset %d", offset)
			}
		}
		zero := bytes.Clone(encoded)
		clear(zero[8:])
		for _, bad := range [][]byte{encoded[:15], append(bytes.Clone(encoded), 0), zero} {
			if _, err := parse(bad); err == nil {
				t.Fatal("accepted malformed probe")
			}
		}
	}
	frame := ErrorFrame{Code: ErrorResumeStale}
	encoded, err := frame.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseError(encoded); err != nil || got != frame {
		t.Fatalf("frame=%+v err=%v", got, err)
	}
}
