package protocolcontract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"encoding/binary"
	"slices"
	"testing"
)

type identityAxis struct {
	name   string
	offset int
}

func TestSenderObjectVectorsAuthenticateAndOpen(t *testing.T) {
	f := newFixture(t)
	for _, object := range buildSenderObjects(t, f) {
		t.Run(object.domain, func(t *testing.T) {
			assertSenderObjectAuthenticates(t, f, object)
			for _, axis := range senderObjectContextAxes(object) {
				t.Run(axis.name, func(t *testing.T) {
					mutatedContext := slices.Clone(object.context)
					mutatedContext[axis.offset] ^= 1
					assertSenderObjectContextRejected(t, f, object, mutatedContext)
				})
			}
			wrongKey := slices.Clone(object.key)
			wrongKey[0] ^= 1
			assertSenderObjectKeyRejected(t, object, wrongKey)
		})
	}
}

func TestOperationEnvelopeAuthenticationBindsEveryAxis(t *testing.T) {
	key := fixed(0x11, 32)
	shareInstance := fixed(0x31, 16)
	protocolSessionID := fixed(0x51, 16)
	laneID := uint32(0x01020304)
	laneEpoch := uint32(7)
	direction := byte(1)
	sequence := uint64(9)
	plain := canonical(t, map[uint64]any{0: uint64(10), 1: fixed(0x71, 16), 2: map[uint64]any{0: uint64(1)}})
	envelope, _ := sealEnvelope(
		t, key, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, fixed(0x91, 12), plain,
	)

	mutations := map[string]func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32){
		"share-instance": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return flipped(shareInstance), protocolSessionID, laneID, laneEpoch, direction, sequence, envelopeCiphertextLength(envelope)
		},
		"protocol-session": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, flipped(protocolSessionID), laneID, laneEpoch, direction, sequence, envelopeCiphertextLength(envelope)
		},
		"lane-id": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, protocolSessionID, laneID + 1, laneEpoch, direction, sequence, envelopeCiphertextLength(envelope)
		},
		"lane-epoch": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, protocolSessionID, laneID, laneEpoch + 1, direction, sequence, envelopeCiphertextLength(envelope)
		},
		"direction": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, protocolSessionID, laneID, laneEpoch, direction ^ 1, sequence, envelopeCiphertextLength(envelope)
		},
		"sequence": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence + 1, envelopeCiphertextLength(envelope)
		},
		"ciphertext-length": func() ([]byte, []byte, uint32, uint32, byte, uint64, uint32) {
			return shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, envelopeCiphertextLength(envelope) + 1
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			share, sessionID, lane, epoch, candidateDirection, candidateSequence, ciphertextLength := mutate()
			aad := operationAAD(share, sessionID, lane, epoch, candidateDirection, candidateSequence, ciphertextLength)
			assertEnvelopeOpenFails(t, key, envelope, aad)
		})
	}
	wrongKey := flipped(key)
	assertEnvelopeOpenFails(
		t, wrongKey, envelope,
		operationAAD(shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, envelopeCiphertextLength(envelope)),
	)
}

func TestSenderControlSignatureBindsEveryAxis(t *testing.T) {
	f := newFixture(t)
	shareInstance := f.shareInstance
	protocolSessionID := fixed(0x11, 16)
	operationID := fixed(0x31, 16)
	semanticBody := canonical(t, map[uint64]any{0: uint64(1), 1: uint64(3), 2: uint64(0x3007)})
	body := unsignedControlWrapper(t, semanticBody)
	laneID, laneEpoch, direction, sequence, kind := uint32(3), uint32(5), byte(1), uint64(7), byte(10)
	preimage := controlSignaturePreimage(
		domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, kind, operationID, body,
	)
	signature := ed25519.Sign(f.edPrivate, preimage)

	mutations := map[string][]byte{
		"domain": controlSignaturePreimage(
			domainTerminal, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, kind, operationID, body,
		),
		"share-instance": controlSignaturePreimage(
			domainControlOperation, flipped(shareInstance), protocolSessionID, laneID, laneEpoch, direction, sequence, kind, operationID, body,
		),
		"protocol-session": controlSignaturePreimage(
			domainControlOperation, shareInstance, flipped(protocolSessionID), laneID, laneEpoch, direction, sequence, kind, operationID, body,
		),
		"lane-id": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID+1, laneEpoch, direction, sequence, kind, operationID, body,
		),
		"lane-epoch": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch+1, direction, sequence, kind, operationID, body,
		),
		"direction": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction^1, sequence, kind, operationID, body,
		),
		"sequence": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence+1, kind, operationID, body,
		),
		"message-kind": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, kind+1, operationID, body,
		),
		"operation-id": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, kind, flipped(operationID), body,
		),
		"body": controlSignaturePreimage(
			domainControlOperation, shareInstance, protocolSessionID, laneID, laneEpoch, direction, sequence, kind, operationID, flipped(body),
		),
	}
	for name, candidate := range mutations {
		t.Run(name, func(t *testing.T) {
			if ed25519.Verify(f.edPublic, candidate, signature) {
				t.Fatal("signature accepted a mutated delivery axis")
			}
		})
	}
	wrongPublic := slices.Clone(f.edPublic)
	wrongPublic[0] ^= 1
	if ed25519.Verify(wrongPublic, preimage, signature) {
		t.Fatal("signature accepted a different sender key")
	}
}

func assertSenderObjectAuthenticates(t *testing.T, f *fixture, object sealedObject) {
	t.Helper()
	if len(object.encoded) < 84 {
		t.Fatalf("object is shorter than fixed overhead")
	}
	ciphertextLength := int(binary.BigEndian.Uint32(object.encoded[4:8]))
	prefixEnd := 20 + ciphertextLength
	if prefixEnd+ed25519.SignatureSize != len(object.encoded) {
		t.Fatalf("length header does not cover object")
	}
	if !ed25519.Verify(f.edPublic, object.signaturePreimage, object.encoded[prefixEnd:]) {
		t.Fatalf("signature failed")
	}
	block, err := aes.NewCipher(object.key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := aead.Open(nil, object.encoded[8:20], object.encoded[20:prefixEnd], object.aad)
	if err != nil || !bytes.Equal(opened, object.plain) {
		t.Fatalf("open = %x, %v", opened, err)
	}
}

func assertSenderObjectContextRejected(t *testing.T, f *fixture, object sealedObject, context []byte) {
	t.Helper()
	ciphertextLength := int(binary.BigEndian.Uint32(object.encoded[4:8]))
	prefixEnd := 20 + ciphertextLength
	contextHash := hash(context)
	aad := slices.Concat([]byte(object.domain), []byte{0}, contextHash, object.encoded[:8])
	block, err := aes.NewCipher(object.key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aead.Open(nil, object.encoded[8:20], object.encoded[20:prefixEnd], aad); err == nil {
		t.Fatal("AEAD accepted a mutated object identity")
	}
	preimage := slices.Concat([]byte(object.domain), []byte{0}, contextHash, object.encoded[:prefixEnd])
	if ed25519.Verify(f.edPublic, preimage, object.encoded[prefixEnd:]) {
		t.Fatal("signature accepted a mutated object identity")
	}
}

func assertSenderObjectKeyRejected(t *testing.T, object sealedObject, key []byte) {
	t.Helper()
	ciphertextLength := int(binary.BigEndian.Uint32(object.encoded[4:8]))
	prefixEnd := 20 + ciphertextLength
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aead.Open(nil, object.encoded[8:20], object.encoded[20:prefixEnd], object.aad); err == nil {
		t.Fatal("AEAD accepted a different object key")
	}
}

func senderObjectContextAxes(object sealedObject) []identityAxis {
	var axes []identityAxis
	switch object.domain {
	case domainDescriptor:
		axes = []identityAxis{{"suite", 0}, {"pk-hash", 1}, {"share-id", 17}}
	case domainCatalogPage:
		axes = []identityAxis{{"share-instance", 0}, {"directory-id", 16}, {"page-index", 32}}
	case domainDirectoryError:
		axes = []identityAxis{{"share-instance", 0}, {"directory-id", 16}}
	case domainRevision:
		axes = []identityAxis{{"share-instance", 0}, {"file-id", 16}}
	case domainBlockRecord:
		axes = []identityAxis{
			{"share-instance", 0}, {"file-id", 16}, {"file-revision", 32},
			{"local-block-index", 48}, {"data-length", 56},
		}
	case domainOfflineCommit:
		axes = []identityAxis{{"share-instance", 0}}
	default:
		panic("unknown sender object domain")
	}
	for _, axis := range axes {
		if axis.offset >= len(object.context) {
			panic("sender object context layout is shorter than its frozen identity axes")
		}
	}
	return axes
}

func operationAAD(
	shareInstance, protocolSessionID []byte,
	laneID, epoch uint32,
	direction byte,
	sequence uint64,
	ciphertextLength uint32,
) []byte {
	return slices.Concat(
		[]byte(domainOperation), []byte{wireVersion, direction}, shareInstance, protocolSessionID,
		u32(laneID), u32(epoch), u64(sequence), u32(ciphertextLength),
	)
}

func envelopeCiphertextLength(envelope []byte) uint32 {
	return binary.BigEndian.Uint32(envelope[12:16])
}

func assertEnvelopeOpenFails(t *testing.T, key, envelope, aad []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aead.Open(nil, envelope[16:28], envelope[28:], aad); err == nil {
		t.Fatal("operation envelope accepted a mutated delivery axis")
	}
}

func controlSignaturePreimage(
	domain string,
	shareInstance, protocolSessionID []byte,
	laneID, laneEpoch uint32,
	direction byte,
	sequence uint64,
	messageKind byte,
	operationID, unsignedBody []byte,
) []byte {
	if operationID == nil {
		operationID = make([]byte, 16)
	}
	return slices.Concat(
		[]byte(domain), []byte{0}, shareInstance, protocolSessionID, u32(laneID), u32(laneEpoch),
		[]byte{direction}, u64(sequence), []byte{messageKind}, operationID, hash(unsignedBody),
	)
}

func flipped(value []byte) []byte {
	result := slices.Clone(value)
	result[0] ^= 1
	return result
}
