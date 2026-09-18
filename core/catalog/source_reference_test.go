package catalog

import (
	"bytes"
	"testing"
)

func TestPrivateSourceReferenceRetainsOpaqueBytesInDurableStorage(t *testing.T) {
	raw := []byte{0xff, 0, '/', '.', '.', ':', 0x91}
	reference, err := NewSourceReference(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 0
	exported := reference.Bytes()
	exported[1] = 1
	expected := []byte{0xff, 0, '/', '.', '.', ':', 0x91}
	if !bytes.Equal(reference.Bytes(), expected) {
		t.Fatal("source reference borrowed caller-owned bytes")
	}
	identity, _ := NewSourceIdentity([]byte("document-id"))
	candidate, _ := NewVersionCandidate([]byte("generation-7"))
	record, err := NewFileNodeRecord(FileID{1}, DirectoryID{2}, "report.bin", reference, identity, candidate, 27, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeNodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := decodeNodeRecord(encoded)
	if err != nil || recovered != record {
		t.Fatalf("opaque private record recovery = %+v, %v", recovered, err)
	}
	want := CatalogNodeMemoryOverhead + uint64(len("report.bin")+len(expected)+len(identity.Bytes())+len(candidate.Bytes()))
	if record.EstimatedMemoryBytes() != want {
		t.Fatalf("source reference not charged exactly once: got=%d want=%d", record.EstimatedMemoryBytes(), want)
	}
}

func TestPrivateSourceReferenceRejectsMissingAndOversizedStorage(t *testing.T) {
	for _, raw := range [][]byte{nil, make([]byte, MaxSourceReferenceBytes+1)} {
		if _, err := NewSourceReference(raw); err == nil {
			t.Fatal("invalid source reference accepted")
		}
	}
	reference, err := NewSourceReference(make([]byte, MaxSourceReferenceBytes))
	if err != nil || reference.IsZero() {
		t.Fatalf("maximum source reference = %v", err)
	}
	record := storedNode{Schema: catalogStorageSchema, Kind: uint8(NodeKindFile), ID: FileID{1}.Bytes(), Parent: DirectoryID{2}.Bytes(), Name: "file.bin", SourceReference: make([]byte, MaxSourceReferenceBytes+1), SourceIdentity: []byte("id"), VersionCandidate: []byte("version")}
	encoded, err := catalogStorageEnc.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNodeRecord(encoded); err == nil {
		t.Fatal("oversized durable reference accepted")
	}
}
