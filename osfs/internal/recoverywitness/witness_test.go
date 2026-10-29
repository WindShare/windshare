package recoverywitness

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"testing"
)

func TestWitnessSeparatesOwnershipFromPublicationAndRejectsCorruption(t *testing.T) {
	witness, err := New(5, bytes.NewReader(bytes.Repeat([]byte{0x71}, nonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []Witness{witness, witness.ForPublication(sha256.Sum256([]byte("hello")))} {
		actual, err := Decode(expected.Encode())
		if err != nil || actual != expected {
			t.Fatalf("round trip: %+v %v", actual, err)
		}
		encoded := expected.Encode()
		for index := range encoded {
			damaged := bytes.Clone(encoded)
			damaged[index] ^= 1
			if _, err := Decode(damaged); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted corrupted byte %d", index)
			}
		}
		physical := append([]byte("hello"), expected.Trailer()...)
		if matched, err := expected.MatchesTrailer(bytes.NewReader(physical), uint64(len(physical))); err != nil || !matched {
			t.Fatalf("token lost after publication preparation: %v %v", matched, err)
		}
		if matched, err := expected.MatchesTrailer(bytes.NewReader(physical), 5); err != nil || matched {
			t.Fatalf("payload alone granted deletion: %v %v", matched, err)
		}
		physical[len(physical)-1] ^= 1
		if matched, err := expected.MatchesTrailer(bytes.NewReader(physical), uint64(len(physical))); err != nil || matched {
			t.Fatalf("corrupted token granted ownership: %v %v", matched, err)
		}
	}
	for _, size := range []uint64{math.MaxInt64, math.MaxUint64} {
		if _, err := New(size, bytes.NewReader([]byte{1})); !errors.Is(err, ErrInvalid) {
			t.Fatal("trailer offset overflow")
		}
	}
	if _, err := New(0, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing random source")
	}
	if _, err := New(0, bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatal("random failure lost")
	}
	if _, err := New(0, bytes.NewReader(make([]byte, nonceBytes))); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero token accepted")
	}
	if encoded := (Witness{}).Encode(); encoded != nil {
		t.Fatal("invalid witness encoded")
	}
	if _, err := Decode(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty witness accepted")
	}
}

type sampledReader struct{ reads, bytes int }

func (reader *sampledReader) ReadAt(p []byte, offset int64) (int, error) {
	reader.reads++
	reader.bytes += len(p)
	for i := range p {
		p[i] = byte(uint64(offset) + uint64(i))
	}
	return len(p), nil
}

func TestPublicationFingerprintHasBoundedIO(t *testing.T) {
	reader := &sampledReader{}
	first, err := PublicationFingerprint(reader, 1<<40)
	if err != nil || reader.reads != 3 || reader.bytes != 3*int(sampleBytes) {
		t.Fatalf("unbounded sampling: %+v %v", reader, err)
	}
	second, err := PublicationFingerprint(&sampledReader{}, 1<<40)
	if err != nil || first != second {
		t.Fatal("fingerprint depends on native identity")
	}
	for _, data := range []string{"", "data", "user"} {
		fingerprint, err := PublicationFingerprint(bytes.NewReader([]byte(data)), uint64(len(data)))
		if err != nil || fingerprint == first {
			t.Fatalf("wrong sized payload matched: %v", err)
		}
	}
}
