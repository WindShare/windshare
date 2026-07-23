package catalog

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func idValue[T ~[IdentityBytes]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func committedRootForTest(t *testing.T, share ShareInstance, root DirectoryID) CommittedRoot {
	t.Helper()
	backend := NewMemoryCatalogBackend()
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, Backend: backend,
		ProcessBudget: generousBudget(t, "descriptor-process"), ShareBudget: generousBudget(t, "descriptor-share"),
		PageSealer: semanticTestCommitter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	childID := idValue[DirectoryID](0xfe)
	if childID == root {
		childID[1] = 1
	}
	locator, _ := NewLocator(0, "")
	identity, _ := NewSourceIdentity([]byte("descriptor-selected-root"))
	child, err := NewDirectoryNodeRecord(childID, root, "selected", locator, identity, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: root, Generation: idValue[DirectoryGeneration](0xfd),
		SelectedRoots: []NodeRecord{child},
	})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitSyntheticRoot(context.Background(), commit, generousBudget(t, "descriptor-startup"))
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func TestIdentityParsingAndKindProjection(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, IdentityBytes)
	directory, err := DirectoryIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	file, err := FileIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if directory.NodeID() != file.NodeID() {
		t.Fatal("kind-safe identities with the same opaque value must share the same NodeID projection")
	}
	raw[0] = 0
	if directory.Bytes()[0] != 0x42 {
		t.Fatal("identity retained caller-owned bytes")
	}
	if _, err := ShareInstanceFromBytes(make([]byte, IdentityBytes-1)); !errors.Is(err, ErrIdentityLength) {
		t.Fatalf("short identity error = %v", err)
	}
	if !(DirectoryID{}).IsZero() || (directory).IsZero() {
		t.Fatal("zero identity reporting is inconsistent")
	}
}

func TestShareDescriptorOwnsInputsAndRejectsInvalidDomainValues(t *testing.T) {
	publicKey := bytes.Repeat([]byte{0x5a}, SenderPublicKeySize)
	wantPublicKey := append([]byte(nil), publicKey...)
	share := idValue[ShareInstance](1)
	root := idValue[DirectoryID](2)
	committed := committedRootForTest(t, share, root)
	descriptor, err := NewShareDescriptor(DescriptorSpec{
		WireVersion:      2,
		Suite:            2,
		ShareInstance:    share,
		SyntheticRoot:    root,
		RootCommit:       committed,
		ChunkSize:        DefaultChunkSize,
		Capabilities:     CapabilityCatalog | CapabilityRanges,
		SenderPublicKey:  publicKey,
		CreatedAtSeconds: 123,
		PathPolicy:       PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	publicKey[0] = 'X'
	returned := descriptor.SenderPublicKey()
	returned[0] = 'Y'
	if !bytes.Equal(descriptor.SenderPublicKey(), wantPublicKey) {
		t.Fatal("descriptor did not own public-key bytes")
	}
	if descriptor.BlockCountFieldPresent() {
		t.Fatal("descriptor must not expose a redundant block count")
	}

	invalid := []DescriptorSpec{
		{},
		{WireVersion: 2, Suite: 2, ShareInstance: share, SyntheticRoot: root, RootCommit: committed, ChunkSize: 1000, SenderPublicKey: bytes.Repeat([]byte{1}, SenderPublicKeySize), PathPolicy: PathPolicyV1},
		{WireVersion: 2, Suite: 2, ShareInstance: share, SyntheticRoot: root, RootCommit: committed, ChunkSize: DefaultChunkSize, PathPolicy: PathPolicyV1},
	}
	for index, spec := range invalid {
		if _, err := NewShareDescriptor(spec); err == nil {
			t.Fatalf("invalid descriptor %d was accepted", index)
		}
	}
}

func TestEntryAndNodeRecordKeepPublicAndPrivateMetadataSeparate(t *testing.T) {
	modified, err := NewModifiedTime(0, 123_000_000, TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	fileID := idValue[FileID](9)
	parentID := idValue[DirectoryID](8)
	entry, err := NewFileEntry(fileID, "report.txt", 42, modified)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := entry.FileID(); !ok || got != fileID {
		t.Fatalf("file identity = %v, %v", got, ok)
	}
	if _, ok := entry.DirectoryID(); ok {
		t.Fatal("file entry projected as a directory")
	}

	locator, _ := NewLocator(0, "root-relative locator")
	identity, _ := NewSourceIdentity([]byte("source identity"))
	candidate, _ := NewVersionCandidate([]byte("version candidate"))
	record, err := NewFileNodeRecord(fileID, parentID, "report.txt", locator, identity, candidate, 42, modified)
	if err != nil {
		t.Fatal(err)
	}
	if !record.MatchesEntry(entry) {
		t.Fatal("node record did not match its public catalog entry")
	}
	if got := record.VersionCandidate().Bytes(); string(got) != "version candidate" {
		t.Fatalf("candidate = %q", got)
	}
}

func TestCanonicalNameAndPathPolicy(t *testing.T) {
	canonical, err := CanonicalName("Cafe\u0301")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "Café" {
		t.Fatalf("canonical name = %q", canonical)
	}
	for _, name := range []string{"", ".", "..", "a/b", "CON", "CONOUT$.txt", "COM¹.log", "name.", "a\u202e", string([]byte{0xff})} {
		if _, err := CanonicalName(name); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("CanonicalName(%q) error = %v", name, err)
		}
	}
	tooDeep := "a"
	for range MaxPathDepth {
		tooDeep += "/a"
	}
	if _, err := CanonicalPath(tooDeep); !errors.Is(err, ErrPathTooDeep) {
		t.Fatalf("deep path error = %v", err)
	}

	decomposed := "Cafe\u0301/report.txt"
	locator, err := NewLocator(0, decomposed)
	if err != nil {
		t.Fatal(err)
	}
	if locator.RelativePath() != decomposed {
		t.Fatalf("private locator spelling was rewritten to %q", locator.RelativePath())
	}
	if canonical, err := CanonicalPath(decomposed); err != nil || canonical != "Café/report.txt" {
		t.Fatalf("public canonical path = %q, %v", canonical, err)
	}
	if key := siblingCollisionKey("ǰ"); key != "ǰ" {
		t.Fatalf("case-fold collision key was not recomposed to NFC: %q", key)
	}
	if _, err := CanonicalPath(".WSRESUME-state/file"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("reserved output-root path = %v", err)
	}
	if nested, err := CanonicalPath("folder/.WSRESUME-state"); err != nil || nested != "folder/.WSRESUME-state" {
		t.Fatalf("nested output-prefix path = %q, %v", nested, err)
	}
}

func TestDomainAccessorsPreserveTypedSemantics(t *testing.T) {
	raw := bytes.Repeat([]byte{0x31}, IdentityBytes)
	node, _ := NodeIDFromBytes(raw)
	generation, _ := DirectoryGenerationFromBytes(raw)
	attempt, _ := ScanAttemptIDFromBytes(raw)
	share, _ := ShareInstanceFromBytes(raw)
	file, _ := FileIDFromBytes(raw)
	directory, _ := DirectoryIDFromBytes(raw)
	if !node.Equal(NodeID(file)) || !generation.Equal(DirectoryGeneration(attempt)) || !attempt.Equal(ScanAttemptID(share)) ||
		!share.Equal(ShareInstance(directory)) || !file.Equal(FileID(directory)) || !directory.Equal(DirectoryID(file)) {
		t.Fatal("constant-time identity equality changed an opaque identity")
	}
	if len(generation.Bytes()) != IdentityBytes || len(attempt.Bytes()) != IdentityBytes {
		t.Fatal("identity accessors changed the fixed width")
	}

	modified, _ := NewModifiedTime(2, 0, TimePrecisionSeconds)
	if !modified.Present() || modified.Seconds() != 2 || modified.Nanoseconds() != 0 || modified.Precision() != TimePrecisionSeconds {
		t.Fatalf("modified time = %+v", modified)
	}
	descriptor, err := NewShareDescriptor(DescriptorSpec{
		WireVersion: WireVersionV2, Suite: SuiteV2, ShareInstance: share, SyntheticRoot: directory,
		RootCommit: committedRootForTest(t, share, directory),
		ChunkSize:  DefaultChunkSize, Capabilities: CapabilityCatalog,
		SenderPublicKey: bytes.Repeat([]byte{7}, SenderPublicKeySize), CreatedAtSeconds: 9, PathPolicy: PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.WireVersion() != WireVersionV2 || descriptor.Suite() != SuiteV2 || descriptor.ShareInstance() != share ||
		descriptor.SyntheticRoot() != directory || descriptor.ChunkSize() != DefaultChunkSize ||
		descriptor.Capabilities() != CapabilityCatalog || descriptor.CreatedAtSeconds() != 9 || descriptor.PathPolicy() != PathPolicyV1 {
		t.Fatalf("descriptor accessors = %+v", descriptor)
	}

	locator, _ := NewLocator(3, "folder/file")
	identity, _ := NewSourceIdentity([]byte("source"))
	candidate, _ := NewVersionCandidate([]byte("candidate"))
	parent := idValue[DirectoryID](8)
	record, err := NewFileNodeRecord(file, parent, "file", locator, identity, candidate, 12, modified)
	if err != nil {
		t.Fatal(err)
	}
	entry := record.Entry()
	if locator.RootSlot() != 3 || locator.RelativePath() != "folder/file" || string(identity.Bytes()) != "source" ||
		record.Kind() != NodeKindFile || record.NodeID() != file.NodeID() || record.Parent() != parent || record.Locator() != locator ||
		record.SourceIdentity() != identity || record.IsSyntheticRoot() || entry.Kind() != NodeKindFile || entry.ExpectedSize() != 12 || entry.ModifiedTime() != modified {
		t.Fatalf("record accessors = %+v / %+v", record, entry)
	}
	if got, ok := record.FileID(); !ok || got != file {
		t.Fatalf("record file projection = %v, %v", got, ok)
	}
}
