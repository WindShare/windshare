package records

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type contentPayloadVectors struct {
	Cases []contentPayloadVector `json:"cases"`
}

type contentPayloadVector struct {
	Name             string `json:"name"`
	CanonicalCBORB64 string `json:"canonicalCborB64"`
}

func TestSessionPayloadsMatchCanonicalCrossRuntimeVectors(t *testing.T) {
	byName := readPayloadVectors(t)
	// The record carries both identities. They become expectations for this
	// codec test after reading the independent fixture values.
	revision := byName["file-revision"]
	if len(revision) == 0 {
		t.Fatal("revision vector is missing")
	}
	fields, err := decodeMap(revision, 9)
	if err != nil {
		t.Fatal(err)
	}
	share, _ := decodeShare(fields[1])
	file, _ := decodeFile(fields[2])
	decoder := Decoder{ShareInstance: share}
	descriptor, err := decoder.DecodeRevision(file, catalog.DefaultChunkSize, revision)
	if err != nil {
		t.Fatal(err)
	}
	block := byName["block-record"]
	decoded, err := decoder.DecodeBlock(descriptor, 2, block)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := (Encoder{}).EncodeBlock(decoded)
	if err != nil || !bytes.Equal(encoded, block) {
		t.Fatalf("canonical block differs: %v", err)
	}
}

func readPayloadVectors(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testvectors", "v2-content-payloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures contentPayloadVectors
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]byte)
	for _, item := range fixtures.Cases {
		result[item.Name] = decodePayloadVector(t, item.CanonicalCBORB64)
	}
	return result
}

func decodePayloadVector(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
