package catalog

import (
	"errors"
	"math"
	"testing"
)

func TestCatalogCodecRejectsHostileWirePages(t *testing.T) {
	parent := idValue[DirectoryID](110)
	file, _ := scannedFile(t, 111, "file", 1).nodeRecord(parent)
	_, validPage, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1), DirectoryID: parent,
		Generation: idValue[DirectoryGeneration](113), Entries: []Entry{file.Entry()}, Terminal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored wireCatalogPage
	if err := pageWireDec.Unmarshal(validPage.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	pageMutations := map[string]func(*wireCatalogPage){
		"schema":   func(value *wireCatalogPage) { value.Schema++ },
		"identity": func(value *wireCatalogPage) { value.Share = nil },
		"entry":    func(value *wireCatalogPage) { value.Entries[0].Name = "." },
		"order": func(value *wireCatalogPage) {
			second := value.Entries[0]
			second.Node = idValue[FileID](114).Bytes()
			value.Entries = []wireCatalogEntry{second, value.Entries[0]}
		},
	}
	for name, mutate := range pageMutations {
		t.Run("page-"+name, func(t *testing.T) {
			value := stored
			value.Entries = append([]wireCatalogEntry(nil), stored.Entries...)
			mutate(&value)
			encoded, err := pageWireEnc.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeCatalogPage(encoded); !errors.Is(err, ErrInvalidPagePayload) {
				t.Fatalf("hostile page accepted: %v", err)
			}
		})
	}
	if _, err := DecodeCatalogPage(nil); !errors.Is(err, ErrInvalidPagePayload) {
		t.Fatalf("empty page accepted: %v", err)
	}
	if _, err := DecodeCatalogPage(make([]byte, MaxCatalogPayloadBytes+1)); !errors.Is(err, ErrInvalidPagePayload) {
		t.Fatalf("oversized page accepted: %v", err)
	}
	mismatched := validPage
	mismatched.address.PageIndex++
	if _, err := mismatched.decode(); !errors.Is(err, ErrInvalidPagePayload) {
		t.Fatalf("address-mismatched payload accepted: %v", err)
	}
}

func TestDirectorySequenceRejectsCrossPageIdentityOrderAndLimitViolations(t *testing.T) {
	share := idValue[ShareInstance](1)
	directory := idValue[DirectoryID](120)
	generation := idValue[DirectoryGeneration](121)
	entryA, _ := NewFileEntry(idValue[FileID](122), "alpha", 1, ModifiedTime{})
	entryZ, _ := NewFileEntry(idValue[FileID](123), "zeta", 1, ModifiedTime{})
	page := func(t *testing.T, spec CatalogPageSpec) CatalogPage {
		t.Helper()
		value, _, err := NewCatalogPage(spec)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{entryZ},
	})
	terminal := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: []Entry{entryA}, Terminal: true,
	})
	var sequence directorySequence
	if err := sequence.accept(CatalogPage{}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("commitment-free page = %v", err)
	}
	if err := sequence.accept(first); err != nil {
		t.Fatal(err)
	}
	if err := sequence.accept(terminal); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("cross-page order = %v", err)
	}

	identityChange := page(t, CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](2), DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: []Entry{entryZ}, Terminal: true,
	})
	sequence = directorySequence{}
	_ = sequence.accept(first)
	if err := sequence.accept(identityChange); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("generation identity change = %v", err)
	}

	gap := terminal
	gap.pageIndex = 2
	sequence = directorySequence{}
	_ = sequence.accept(first)
	if err := sequence.accept(gap); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("page gap = %v", err)
	}

	sequence = directorySequence{identitySet: true, committed: CommittedDirectory{
		shareInstance: share, directoryID: directory, generation: generation, entryCount: MaxDirectoryEntries,
	}}
	if err := sequence.accept(first); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("entry limit = %v", err)
	}
	sequence = directorySequence{}
	overOmitted := first
	overOmitted.terminal = true
	overOmitted.omittedCount = MaxDirectoryEntries
	if err := sequence.accept(overOmitted); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("omitted limit = %v", err)
	}

	sequence = directorySequence{}
	validTerminal := first
	validTerminal.terminal = true
	if err := sequence.accept(validTerminal); err != nil {
		t.Fatal(err)
	}
	if err := sequence.accept(validTerminal); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("page after terminal = %v", err)
	}

	if _, err := NewDirectorySnapshot([]CatalogPage{validTerminal, validTerminal}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("early terminal snapshot = %v", err)
	}
	upper, _ := NewFileEntry(idValue[FileID](124), "Alpha", 1, ModifiedTime{})
	lower, _ := NewFileEntry(idValue[FileID](125), "alpha", 1, ModifiedTime{})
	nameFirst := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{upper},
	})
	nameSecond := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: nameFirst.Commitment(), Entries: []Entry{lower}, Terminal: true,
	})
	if _, err := NewDirectorySnapshot([]CatalogPage{nameFirst, nameSecond}); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("cross-page duplicate name = %v", err)
	}
	sharedID := idValue[FileID](126)
	sharedAlpha, _ := NewFileEntry(sharedID, "alpha", 1, ModifiedTime{})
	sharedBeta, _ := NewFileEntry(sharedID, "beta", 1, ModifiedTime{})
	nodeFirst := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{sharedAlpha},
	})
	nodeSecond := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: nodeFirst.Commitment(), Entries: []Entry{sharedBeta}, Terminal: true,
	})
	if _, err := NewDirectorySnapshot([]CatalogPage{nodeFirst, nodeSecond}); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("cross-page duplicate node = %v", err)
	}

	snapshot, err := NewDirectorySnapshot([]CatalogPage{validTerminal})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ShareInstance() != share || snapshot.DirectoryID() != directory || snapshot.Generation() != generation {
		t.Fatal("snapshot identity accessors changed committed identity")
	}
	other := snapshot
	other.pages = append([]CatalogPage(nil), snapshot.pages...)
	other.pages[0].commitment[0] ^= 0xff
	if snapshot.Equal(other) {
		t.Fatal("snapshots with different commitments compared equal")
	}
	if _, ok := snapshot.Page(math.MaxUint32); ok {
		t.Fatal("out-of-range snapshot page succeeded")
	}
	if (DirectorySnapshot{}).TerminalCommitment() != (PageCommitment{}) {
		t.Fatal("empty snapshot returned a terminal commitment")
	}
}
