package records

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type senderObjectVectors struct {
	Cases []senderObjectVector `json:"cases"`
}

type senderObjectVector struct {
	Domain    string `json:"domain"`
	ObjectB64 string `json:"objectB64"`
}

func TestSenderObjectsMatchFrozenCrossRuntimeVectors(t *testing.T) {
	t.Parallel()
	vectors := readSenderObjectVectors(t)
	sequence := func(first byte, length int) []byte {
		result := make([]byte, length)
		for index := range result {
			result[index] = first + byte(index)
		}
		return result
	}

	share, err := catalog.ShareInstanceFromBytes(sequence(0x40, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	file, err := catalog.FileIDFromBytes(sequence(0x70, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	revision, err := content.FileRevisionFromBytes(sequence(0x90, content.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	geometry, err := content.NewFileGeometry(2_097_175, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_200, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, modified)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := content.NewKeyTree(sequence(0, content.ReadSecretBytes), share)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(sequence(0x20, ed25519.SeedSize))
	nonces := append(sequence(0xe8, ObjectNonceBytes), sequence(0xf4, ObjectNonceBytes)...)
	sealer, err := NewSealer(SealerConfig{
		ShareInstance: share,
		Keys:          keys,
		SigningKey:    privateKey,
		NonceSource:   bytes.NewReader(nonces),
	})
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewOpener(OpenerConfig{
		ShareInstance:   share,
		Keys:            keys,
		VerificationKey: privateKey.Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}

	revisionObject, err := sealer.SealRevision(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	assertSenderObjectVector(t, vectors, RevisionObjectDomain, revisionObject)
	if opened, err := opener.OpenRevision(file, catalog.DefaultChunkSize, revisionObject); err != nil || opened != descriptor {
		t.Fatalf("open frozen revision vector: descriptor=%+v err=%v", opened, err)
	}

	block, err := NewBlockRecord(descriptor, 2, []byte("WindShare v2 last block"))
	if err != nil {
		t.Fatal(err)
	}
	sealedBlock, err := sealer.SealBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	assertSenderObjectVector(t, vectors, BlockRecordDomain, sealedBlock.Object)
	if opened, err := opener.OpenBlock(descriptor, 2, sealedBlock.Object); err != nil || !bytes.Equal(opened.Data(), block.Data()) {
		t.Fatalf("open frozen block vector: data=%x err=%v", opened.Data(), err)
	}
}

func readSenderObjectVectors(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join("..", "..", "testvectors", "v2-sender-objects.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sender object vectors: %v", err)
	}
	var file senderObjectVectors
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode sender object vectors: %v", err)
	}
	result := make(map[string][]byte, len(file.Cases))
	for _, vector := range file.Cases {
		object, err := base64.StdEncoding.DecodeString(vector.ObjectB64)
		if err != nil {
			t.Fatalf("decode %q object: %v", vector.Domain, err)
		}
		result[vector.Domain] = object
	}
	return result
}

func assertSenderObjectVector(t *testing.T, vectors map[string][]byte, domain string, actual []byte) {
	t.Helper()
	expected, ok := vectors[domain]
	if !ok {
		t.Fatalf("sender object vector %q is missing", domain)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("sender object %q diverged from the frozen cross-runtime bytes\nactual:   %x\nexpected: %x", domain, actual, expected)
	}
}
