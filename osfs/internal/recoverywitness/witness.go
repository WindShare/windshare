// Package recoverywitness encodes portable partial ownership and publication evidence.
package recoverywitness

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const (
	domain     = "windshare/owned-stage/v1\x00"
	nonceBytes = sha256.Size
	// A stage carries its nonce outside the logical payload until publication.
	TrailerSize           = len(domain) + nonceBytes + 8 + sha256.Size
	EncodedSize           = TrailerSize + 1 + sha256.Size
	partialPhase     byte = 1
	publicationPhase byte = 2
)

var ErrInvalid = errors.New("invalid portable recovery witness")

type Witness struct {
	nonce       [nonceBytes]byte
	size        uint64
	phase       byte
	fingerprint [sha256.Size]byte
}

func New(size uint64, random io.Reader) (Witness, error) {
	if size > uint64(math.MaxInt64-TrailerSize) || random == nil {
		return Witness{}, ErrInvalid
	}
	w := Witness{size: size, phase: partialPhase}
	if _, err := io.ReadFull(random, w.nonce[:]); err != nil {
		return Witness{}, err
	}
	if w.nonce == ([nonceBytes]byte{}) {
		return Witness{}, ErrInvalid
	}
	return w, nil
}

func (w Witness) Size() uint64                   { return w.size }
func (w Witness) Publishing() bool               { return w.phase == publicationPhase }
func (w Witness) Fingerprint() [sha256.Size]byte { return w.fingerprint }

func (w Witness) ForPublication(fingerprint [sha256.Size]byte) Witness {
	w.phase, w.fingerprint = publicationPhase, fingerprint
	return w
}

func (w Witness) valid() bool {
	return w.nonce != ([nonceBytes]byte{}) && w.size <= uint64(math.MaxInt64-TrailerSize) &&
		(w.phase == partialPhase && w.fingerprint == ([sha256.Size]byte{}) ||
			w.phase == publicationPhase && w.fingerprint != ([sha256.Size]byte{}))
}

func (w Witness) Trailer() []byte {
	body := append([]byte(domain), w.nonce[:]...)
	body = binary.BigEndian.AppendUint64(body, w.size)
	sum := sha256.Sum256(body)
	return append(body, sum[:]...)
}

func (w Witness) Encode() []byte {
	if !w.valid() {
		return nil
	}
	body := append([]byte(domain), w.nonce[:]...)
	body = binary.BigEndian.AppendUint64(body, w.size)
	body = append(body, w.phase)
	body = append(body, w.fingerprint[:]...)
	sum := sha256.Sum256(body)
	return append(body, sum[:]...)
}

func Decode(encoded []byte) (Witness, error) {
	if len(encoded) != EncodedSize || !bytes.HasPrefix(encoded, []byte(domain)) {
		return Witness{}, ErrInvalid
	}
	body := encoded[:len(encoded)-sha256.Size]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], encoded[len(body):]) {
		return Witness{}, ErrInvalid
	}
	offset := len(domain)
	var w Witness
	copy(w.nonce[:], body[offset:offset+nonceBytes])
	offset += nonceBytes
	w.size = binary.BigEndian.Uint64(body[offset : offset+8])
	offset += 8
	w.phase = body[offset]
	copy(w.fingerprint[:], body[offset+1:])
	if !w.valid() {
		return Witness{}, ErrInvalid
	}
	return w, nil
}

func (w Witness) MatchesTrailer(reader io.ReaderAt, physicalSize uint64) (bool, error) {
	if !w.valid() || physicalSize != w.size+uint64(TrailerSize) {
		return false, nil
	}
	actual := make([]byte, TrailerSize)
	_, err := reader.ReadAt(actual, int64(w.size))
	return err == nil && bytes.Equal(actual, w.Trailer()), err
}
