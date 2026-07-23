package catalog

import (
	"context"
	"errors"
	"testing"
)

func selectedRootRecord(t *testing.T, root DirectoryID, id byte, name string) NodeRecord {
	t.Helper()
	locator, _ := NewLocator(RootSlot(id), "")
	identity, _ := NewSourceIdentity([]byte(name))
	record, err := NewDirectoryNodeRecord(idValue[DirectoryID](id), root, name, locator, identity, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestSyntheticRootCommitSortsOnlySelectedRootRecords(t *testing.T) {
	root := idValue[DirectoryID](20)
	commit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: idValue[ShareInstance](1), SyntheticRoot: root, Generation: idValue[DirectoryGeneration](2),
		SelectedRoots: []NodeRecord{selectedRootRecord(t, root, 4, "zeta"), selectedRootRecord(t, root, 3, "alpha")},
	})
	if err != nil {
		t.Fatal(err)
	}
	iterator, err := commit.children.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer iterator.Close()
	first, ok, err := iterator.Next(context.Background())
	if err != nil || !ok {
		t.Fatal("missing first selected root")
	}
	second, ok, err := iterator.Next(context.Background())
	if err != nil || !ok {
		t.Fatal("missing second selected root")
	}
	if first.Entry().Name() != "alpha" || second.Entry().Name() != "zeta" || !commit.Directory().IsSyntheticRoot() {
		t.Fatalf("synthetic root commit = %+v", commit)
	}
}

func TestSyntheticRootCommitRejectsPortableNameCollisions(t *testing.T) {
	root := idValue[DirectoryID](20)
	_, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: idValue[ShareInstance](1), SyntheticRoot: root, Generation: idValue[DirectoryGeneration](2),
		SelectedRoots: []NodeRecord{selectedRootRecord(t, root, 3, "Readme"), selectedRootRecord(t, root, 4, "README")},
	})
	if err == nil {
		t.Fatal("portable selected-root collision was accepted")
	}
}

func TestSyntheticRootCommitRejectsOutputReservedRootName(t *testing.T) {
	root := idValue[DirectoryID](20)
	_, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: idValue[ShareInstance](1), SyntheticRoot: root, Generation: idValue[DirectoryGeneration](2),
		SelectedRoots: []NodeRecord{selectedRootRecord(t, root, 3, ".WSRESUME-state")},
	})
	if err == nil {
		t.Fatal("output-reserved selected-root name was accepted")
	}
}

func TestSyntheticRootCommitRejectsEmptySelection(t *testing.T) {
	_, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: idValue[ShareInstance](1), SyntheticRoot: idValue[DirectoryID](2),
		Generation: idValue[DirectoryGeneration](3),
	})
	if err == nil {
		t.Fatal("empty selected-root transaction was accepted")
	}
	commit := rootCommit(t, idValue[ShareInstance](1), idValue[DirectoryID](2), idValue[DirectoryGeneration](3), nil)
	if err := validateDirectoryCommit(idValue[ShareInstance](1), commit, true); err == nil {
		t.Fatal("generic directory commit bypassed the synthetic-root minimum")
	}
}

func TestSyntheticRootCommitRejectsOmittedSelections(t *testing.T) {
	share := idValue[ShareInstance](1)
	root := idValue[DirectoryID](2)
	child := selectedRootRecord(t, root, 3, "selected")
	commit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: root, Generation: idValue[DirectoryGeneration](4),
		SelectedRoots: []NodeRecord{child},
	})
	if err != nil {
		t.Fatal(err)
	}
	commit.omittedCount = 1
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	if _, err := store.CommitSyntheticRoot(context.Background(), commit, generousBudget(t, "startup")); err == nil {
		t.Fatal("synthetic root hid selected roots behind OmittedCount")
	}
}

func TestConflictingSyntheticRootCannotPublishBeforeAuthorityCheck(t *testing.T) {
	share := idValue[ShareInstance](1)
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	startup := generousBudget(t, "startup")
	firstRoot := idValue[DirectoryID](30)
	first := rootCommit(
		t, share, firstRoot, idValue[DirectoryGeneration](31),
		[]NodeRecord{selectedRootRecord(t, firstRoot, 1, "first")},
	)
	commitSyntheticRoot(t, store, first, startup)
	secondRoot := idValue[DirectoryID](32)
	second := rootCommit(
		t, share, secondRoot, idValue[DirectoryGeneration](33),
		[]NodeRecord{selectedRootRecord(t, secondRoot, 2, "second")},
	)
	if _, err := store.CommitSyntheticRoot(context.Background(), second, startup); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("conflicting root authority = %v", err)
	}
	if _, found, err := store.Directory(context.Background(), secondRoot); err != nil || found {
		t.Fatalf("conflicting synthetic root was published: found=%v err=%v", found, err)
	}
}
