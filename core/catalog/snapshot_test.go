package catalog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

type testFailureEncoder struct{}

func (testFailureEncoder) EncodeFailure(record DirectoryFailureRecord) (FailurePayload, error) {
	var encoded bytes.Buffer
	_, _ = encoded.Write(record.ShareInstance().Bytes())
	_, _ = encoded.Write(record.DirectoryID().Bytes())
	_, _ = encoded.Write(record.AttemptID().Bytes())
	_, _ = encoded.Write(record.Generation().Bytes())
	_, _ = encoded.Write(record.PreviousAttemptID().Bytes())
	_ = encoded.WriteByte(byte(record.Kind()))
	var retry [8]byte
	binary.BigEndian.PutUint64(retry[:], uint64(record.RetryAfter()/time.Millisecond))
	_, _ = encoded.Write(retry[:])
	return NewFailurePayload(encoded.Bytes())
}

func mustTestPagePayload(t *testing.T, page CatalogPage) PagePayload {
	t.Helper()
	payload, err := encodePagePayload(page)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func testEntries(t *testing.T, start, count int) []Entry {
	t.Helper()
	entries := make([]Entry, count)
	for i := range count {
		name := string(rune('a' + start + i))
		entry, err := NewFileEntry(idValue[FileID](byte(start+i+1)), name, uint64(i), ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = entry
	}
	return entries
}

func TestDirectorySnapshotValidatesPageChainAndOwnsEntries(t *testing.T) {
	instance := idValue[ShareInstance](1)
	directory := idValue[DirectoryID](2)
	generation := idValue[DirectoryGeneration](3)
	first, _, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance,
		DirectoryID:   directory,
		Generation:    generation,
		PageIndex:     0,
		Entries:       testEntries(t, 0, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEntries := testEntries(t, 2, 1)
	second, _, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance,
		DirectoryID:   directory,
		Generation:    generation,
		PageIndex:     1,
		Terminal:      true,
		Previous:      first.Commitment(),
		Entries:       secondEntries,
		OmittedCount:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondEntries[0] = Entry{}

	snapshot, err := NewDirectorySnapshot([]CatalogPage{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EntryCount() != 3 || snapshot.OmittedCount() != 4 || snapshot.PageCount() != 2 || snapshot.TerminalCommitment() != second.Commitment() {
		t.Fatalf("snapshot geometry = entries %d pages %d", snapshot.EntryCount(), snapshot.PageCount())
	}
	if first.ShareInstance() != instance || first.DirectoryID() != directory || first.Generation() != generation ||
		first.PageIndex() != 0 || !first.Previous().IsZero() || first.Terminal() || first.OmittedCount() != 0 {
		t.Fatalf("page accessors = %+v", first)
	}
	wantMemory := uint64(CatalogPageMemoryOverhead + 2*(CatalogEntryMemoryOverhead+CatalogNameMemoryOverhead+1))
	if got := first.EstimatedMemoryBytes(); got != wantMemory {
		t.Fatalf("page memory charge = %d, want %d", got, wantMemory)
	}
	wantSnapshotMemory := uint64(DirectorySnapshotMemoryOverhead) + first.EstimatedMemoryBytes() + second.EstimatedMemoryBytes()
	if got := snapshot.EstimatedMemoryBytes(); got != wantSnapshotMemory {
		t.Fatalf("snapshot memory charge = %d, want %d", got, wantSnapshotMemory)
	}
	if !snapshot.Equal(snapshot) {
		t.Fatal("snapshot must equal itself")
	}
	if page, ok := snapshot.Page(1); !ok || page.Commitment() != second.Commitment() {
		t.Fatalf("indexed page = present %v commitment %x", ok, page.Commitment())
	}
	if _, ok := snapshot.Page(2); ok {
		t.Fatal("out-of-range indexed page was reported present")
	}
	pages := snapshot.Pages()
	pages[0] = CatalogPage{}
	if snapshot.Pages()[0].Commitment() != first.Commitment() {
		t.Fatal("snapshot exposed mutable page storage")
	}
}

func TestDirectorySnapshotRejectsGapsConflictsAndNonTerminalTail(t *testing.T) {
	instance := idValue[ShareInstance](1)
	directory := idValue[DirectoryID](2)
	generation := idValue[DirectoryGeneration](3)
	page, _, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance,
		DirectoryID:   directory,
		Generation:    generation,
		PageIndex:     1,
		Previous:      mustPageCommitment(t, bytes.Repeat([]byte{0x7f}, PageCommitmentBytes)),
		Terminal:      true,
		Entries:       testEntries(t, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectorySnapshot([]CatalogPage{page}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("gap error = %v", err)
	}

	nonTerminal, _, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance,
		DirectoryID:   directory,
		Generation:    generation,
		Entries:       testEntries(t, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectorySnapshot([]CatalogPage{nonTerminal}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("non-terminal tail error = %v", err)
	}
}

func mustPageCommitment(t *testing.T, raw []byte) PageCommitment {
	t.Helper()
	commitment, err := NewPageCommitment(raw)
	if err != nil {
		t.Fatal(err)
	}
	return commitment
}

func TestCatalogPageEnforcesEntryAndSiblingBounds(t *testing.T) {
	base := CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1),
		DirectoryID:   idValue[DirectoryID](2),
		Generation:    idValue[DirectoryGeneration](3),
		Terminal:      true,
	}
	base.Entries = make([]Entry, MaxCatalogPageEntries+1)
	if _, _, err := NewCatalogPage(base); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("page limit error = %v", err)
	}

	first, _ := NewFileEntry(idValue[FileID](1), "Readme", 1, ModifiedTime{})
	second, _ := NewFileEntry(idValue[FileID](2), "README", 1, ModifiedTime{})
	base.Entries = []Entry{first, second}
	if _, _, err := NewCatalogPage(base); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("collision error = %v", err)
	}
}
