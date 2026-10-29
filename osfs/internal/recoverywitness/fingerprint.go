package recoverywitness

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
)

const sampleBytes uint64 = 4096

// PublicationFingerprint reconciles a move interrupted before its receipt.
// It is bounded content evidence, not object identity or a full integrity scan;
// in particular it never grants deletion authority over a trailerless file.
func PublicationFingerprint(reader io.ReaderAt, size uint64) ([sha256.Size]byte, error) {
	h := sha256.New()
	_, _ = h.Write([]byte("windshare/publication-samples/v1\x00"))
	writeNumber(h, size)
	length := min(size, sampleBytes)
	offsets := [3]uint64{0, (size - length) / 2, size - length}
	previous := ^uint64(0)
	buffer := make([]byte, length)
	for _, offset := range offsets {
		if offset == previous {
			continue
		}
		previous = offset
		if length != 0 {
			if _, err := reader.ReadAt(buffer, int64(offset)); err != nil {
				return [sha256.Size]byte{}, err
			}
		}
		writeNumber(h, offset)
		_, _ = h.Write(buffer)
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result, nil
}

func writeNumber(h hash.Hash, number uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], number)
	_, _ = h.Write(encoded[:])
}
