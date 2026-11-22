package catalog

import (
	"bytes"
	"context"
	"testing"
)

func TestPrivateSourceReferenceRetainsOpaqueBytesInStore(t *testing.T) {
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
	parent := idValue[DirectoryID](2)
	record, err := NewFileNodeRecord(FileID{1}, parent, "report.bin", reference, identity, candidate, 27, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	store, _, _ := newStore(t, nil)
	defer store.Close()
	session := generousBudget(t, "source-reference-session")
	root := idValue[DirectoryID](9)
	parentRecord := selectedDirectory(t, root, 2, "folder")
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](10), []NodeRecord{parentRecord},
	), session)
	commit := DirectoryCommit{
		directory:  parentRecord,
		generation: idValue[DirectoryGeneration](11),
		children:   []NodeRecord{record},
	}
	if _, err := store.CommitDirectory(context.Background(), commit, session); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.Node(context.Background(), record.NodeID())
	if err != nil || !found || stored != record || !bytes.Equal(stored.SourceReference().Bytes(), expected) {
		t.Fatalf("opaque private record round trip = %+v, found=%v, err=%v", stored, found, err)
	}
	want := CatalogNodeMemoryOverhead + uint64(len("report.bin")+len(expected)+len(identity.Bytes())+len(candidate.Bytes()))
	if record.EstimatedMemoryBytes() != want {
		t.Fatalf("source reference not charged exactly once: got=%d want=%d", record.EstimatedMemoryBytes(), want)
	}
}

func TestPrivateSourceReferenceRejectsMissingAndOversizedValues(t *testing.T) {
	for _, raw := range [][]byte{nil, make([]byte, MaxSourceReferenceBytes+1)} {
		if _, err := NewSourceReference(raw); err == nil {
			t.Fatal("invalid source reference accepted")
		}
	}
	reference, err := NewSourceReference(make([]byte, MaxSourceReferenceBytes))
	if err != nil || reference.IsZero() {
		t.Fatalf("maximum source reference = %v", err)
	}
}
