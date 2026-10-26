package outputprobe

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestProbeNamesBindExactlyOneCanonicalEntropyEncoding(t *testing.T) {
	entropy := []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	}
	want := NamePrefix + "000102030405060708090a0b0c0d0e0f"
	got, err := NewName(bytes.NewReader(entropy))
	if err != nil || got != want {
		t.Fatalf("new probe name = %q, %v; want %q", got, err, want)
	}
	if !CanonicalName(got) {
		t.Fatalf("generated probe name is not canonical: %q", got)
	}
	if _, err := NewName(bytes.NewReader(entropy[:len(entropy)-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short entropy error = %v, want unexpected EOF", err)
	}

	for _, name := range []string{
		ReservedPrefix,
		NamePrefix + strings.Repeat("0", RandomBytes*2-1),
		NamePrefix + strings.Repeat("0", RandomBytes*2+1),
		NamePrefix + strings.Repeat("0", RandomBytes*2-1) + "g",
		NamePrefix + strings.Repeat("0", RandomBytes*2-1) + "A",
		ReservedPrefix + "_" + strings.Repeat("0", RandomBytes*2),
	} {
		if CanonicalName(name) {
			t.Errorf("malformed reserved name was canonical: %q", name)
		}
	}
}
