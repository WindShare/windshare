package receivecontract

import (
	"crypto/sha256"
	"encoding/binary"
)

const schemaVersion uint8 = 1

func canonicalRecord(domain string, fields ...[]byte) []byte {
	encoded := make([]byte, 0, len(domain)+2)
	encoded = append(encoded, domain...)
	encoded = append(encoded, 0, schemaVersion)
	for _, field := range fields {
		encoded = append(encoded, field...)
	}
	return encoded
}

func frame(value []byte) []byte {
	encoded := make([]byte, 8, 8+len(value))
	binary.BigEndian.PutUint64(encoded, uint64(len(value)))
	return append(encoded, value...)
}

func uint32Bytes(value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return encoded[:]
}

func uint64Bytes(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func digest(value []byte) [sha256.Size]byte { return sha256.Sum256(value) }

func clone(value []byte) []byte { return append([]byte(nil), value...) }

func nonZero(value []byte) bool {
	var combined byte
	for _, current := range value {
		combined |= current
	}
	return combined != 0
}
