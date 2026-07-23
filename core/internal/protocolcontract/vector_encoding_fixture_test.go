package protocolcontract

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

func canonical(t *testing.T, value any) []byte {
	t.Helper()
	options := cbor.CoreDetEncOptions()
	mode, err := options.EncMode()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := mode.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func unsignedControlWrapper(t *testing.T, semanticBody []byte) []byte {
	t.Helper()
	return canonical(t, map[uint64]any{0: uint64(1), 1: cbor.RawMessage(semanticBody)})
}

func signedControlWrapper(t *testing.T, semanticBody, signature []byte) []byte {
	t.Helper()
	return canonical(t, map[uint64]any{
		0: uint64(1), 1: cbor.RawMessage(semanticBody), 255: signature,
	})
}

func derive(t *testing.T, secret []byte, label string, context []byte) []byte {
	t.Helper()
	info := string(slices.Concat([]byte(label), []byte{0}, context))
	key, err := hkdf.Key(sha256.New, secret, nil, info, 32)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func hash(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func first(data []byte, count int) []byte { return slices.Clone(data[:count]) }
func b64(data []byte) string              { return base64.StdEncoding.EncodeToString(data) }

func fixed(firstByte byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = firstByte + byte(i)
	}
	return result
}

func u32(value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return encoded[:]
}

func u16(value uint16) []byte {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	return encoded[:]
}

func u64(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}
