package catalog

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDomainConstructorsRejectInvalidAxes(t *testing.T) {
	share := idValue[ShareInstance](1)
	root := idValue[DirectoryID](2)
	validDescriptor := DescriptorSpec{
		WireVersion: WireVersionV2, Suite: SuiteV2, ShareInstance: share,
		SyntheticRoot: root, RootCommit: committedRootForTest(t, share, root), ChunkSize: DefaultChunkSize,
		SenderPublicKey: bytes.Repeat([]byte{1}, SenderPublicKeySize), PathPolicy: PathPolicy,
	}
	mutations := []func(*DescriptorSpec){
		func(spec *DescriptorSpec) { spec.Capabilities = 1 << 63 },
		func(spec *DescriptorSpec) { spec.Capabilities = CapabilityShareInstanceRefreshReserved },
		func(spec *DescriptorSpec) { spec.Capabilities = CapabilityOfflineObjectReserved },
		func(spec *DescriptorSpec) { spec.Capabilities = CapabilityMultiRelayDeliveryReserved },
		func(spec *DescriptorSpec) { spec.CreatedAtSeconds = MaxFileSize + 1 },
		func(spec *DescriptorSpec) { spec.PathPolicy = "unknown" },
		func(spec *DescriptorSpec) { spec.SenderPublicKey = spec.SenderPublicKey[:SenderPublicKeySize-1] },
	}
	for index, mutate := range mutations {
		spec := validDescriptor
		mutate(&spec)
		if _, err := NewShareDescriptor(spec); err == nil {
			t.Fatalf("descriptor mutation %d was accepted", index)
		}
	}

	if _, err := NewModifiedTime(1, 1, TimePrecisionSeconds); err == nil {
		t.Fatal("fractional second was accepted")
	}
	if _, err := NewModifiedTime(1, 1, TimePrecisionMilliseconds); err == nil {
		t.Fatal("sub-millisecond value was accepted")
	}
	if _, err := NewSourceIdentity(nil); err == nil {
		t.Fatal("empty source identity was accepted")
	}
	if _, err := NewVersionCandidate(nil); err == nil {
		t.Fatal("empty version candidate was accepted")
	}
	if _, err := NewSourceIdentity(make([]byte, MaxSourceIdentityBytes+1)); err == nil {
		t.Fatal("oversized source identity was accepted")
	}
	if _, err := NewVersionCandidate(make([]byte, MaxVersionCandidateBytes+1)); err == nil {
		t.Fatal("oversized version candidate was accepted")
	}
	if _, err := NewSourceReference(nil); err == nil {
		t.Fatal("empty source reference was accepted")
	}
	if _, err := NewSourceReference(make([]byte, MaxSourceReferenceBytes+1)); err == nil {
		t.Fatal("oversized source reference was accepted")
	}
	if _, err := NewFileEntry(FileID{}, "file", 1, ModifiedTime{}); err == nil {
		t.Fatal("zero file identity was accepted")
	}
	if _, err := NewFileEntry(idValue[FileID](1), "file", MaxFileSize+1, ModifiedTime{}); err == nil {
		t.Fatal("unsafe file size was accepted")
	}
	if _, err := NewDirectoryEntry(DirectoryID{}, "dir", ModifiedTime{}); err == nil {
		t.Fatal("zero directory identity was accepted")
	}
	if _, err := NewSyntheticRootNodeRecord(DirectoryID{}); err == nil {
		t.Fatal("zero synthetic root was accepted")
	}
	locator, _ := NewSourceReference([]byte("object:" + "file"))
	identity, _ := NewSourceIdentity([]byte("identity"))
	candidate, _ := NewVersionCandidate([]byte("candidate"))
	if _, err := NewDirectoryNodeRecord(idValue[DirectoryID](1), DirectoryID{}, "dir", locator, identity, ModifiedTime{}); err == nil {
		t.Fatal("parentless directory record was accepted")
	}
	if _, err := NewDirectoryNodeRecord(idValue[DirectoryID](2), idValue[DirectoryID](2), "dir", locator, identity, ModifiedTime{}); err == nil {
		t.Fatal("self-parenting directory record was accepted")
	}
	if _, err := NewFileNodeRecord(idValue[FileID](2), idValue[DirectoryID](2), "file", locator, identity, candidate, 1, ModifiedTime{}); err == nil {
		t.Fatal("file identity colliding with its parent directory was accepted")
	}
	if _, err := NewFileNodeRecord(idValue[FileID](1), idValue[DirectoryID](2), "file", locator, identity, VersionCandidate{}, 1, ModifiedTime{}); err == nil {
		t.Fatal("candidate-free file record was accepted")
	}
	if string(candidate.Bytes()) != "candidate" || len((NodeID{}).Bytes()) != IdentityBytes {
		t.Fatal("opaque accessors changed their values")
	}
	if len((ScanAttemptID{}).Bytes()) != IdentityBytes || (NodeRecord{syntheticRoot: true}).Entry() != (Entry{}) {
		t.Fatal("zero-value accessors changed their semantic shape")
	}
}

func TestCanonicalPathRejectsEveryContainmentShape(t *testing.T) {
	invalid := []string{"", "/root", "root/", `root\file`, "root//file", strings.Repeat("x", MaxPathBytes+1), string([]byte{0xff})}
	for _, path := range invalid {
		if _, err := CanonicalPath(path); err == nil {
			t.Fatalf("unsafe path %q was accepted", path)
		}
	}
}

func TestSnapshotConstructorsRejectMalformedSemanticChains(t *testing.T) {
	if _, err := NewPageCommitment(make([]byte, PageCommitmentBytes-1)); err == nil {
		t.Fatal("short commitment was accepted")
	}
	base := CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1), DirectoryID: idValue[DirectoryID](2),
		Generation: idValue[DirectoryGeneration](3), Terminal: true,
	}
	if _, _, err := NewCatalogPage(CatalogPageSpec{}); err == nil {
		t.Fatal("identity-free page was accepted")
	}
	withPrevious := base
	withPrevious.Previous = mustPageCommitment(t, bytes.Repeat([]byte{1}, PageCommitmentBytes))
	if _, _, err := NewCatalogPage(withPrevious); err == nil {
		t.Fatal("page zero predecessor was accepted")
	}
	later := base
	later.PageIndex = 1
	if _, _, err := NewCatalogPage(later); err == nil {
		t.Fatal("later page without predecessor was accepted")
	}
	nonterminalEmpty := base
	nonterminalEmpty.Terminal = false
	if _, _, err := NewCatalogPage(nonterminalEmpty); err == nil {
		t.Fatal("empty nonterminal page was accepted")
	}
	unsorted := base
	unsorted.Entries = testEntries(t, 0, 2)
	unsorted.Entries[0], unsorted.Entries[1] = unsorted.Entries[1], unsorted.Entries[0]
	if _, _, err := NewCatalogPage(unsorted); err == nil {
		t.Fatal("unsorted entries were accepted")
	}
	if _, err := NewDirectorySnapshot(nil); err == nil {
		t.Fatal("empty snapshot was accepted")
	}
	omittedOverflow := base
	omittedOverflow.OmittedCount = MaxDirectoryEntries + 1
	overflowPage, _, err := NewCatalogPage(omittedOverflow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirectorySnapshot([]CatalogPage{overflowPage}); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("omitted-entry overflow = %v", err)
	}
	if (DirectorySnapshot{}).TerminalCommitment() != (PageCommitment{}) || (DirectorySnapshot{}).Equal(DirectorySnapshot{directoryID: idValue[DirectoryID](1)}) {
		t.Fatal("zero snapshot semantics are inconsistent")
	}
}

func TestCatalogStoreRejectsCancelledAndClosedReads(t *testing.T) {
	store, _, _ := newStore(t, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Directory(cancelled, idValue[DirectoryID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled directory load = %v", err)
	}
	if _, _, err := store.PagePayload(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page load = %v", err)
	}
	if _, _, err := store.Node(cancelled, idValue[NodeID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled node load = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store.directories != nil || store.nodes != nil || store.failures != nil {
		t.Fatal("closed store retained catalog state after its budget was released")
	}
	if _, _, err := store.Directory(context.Background(), idValue[DirectoryID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed directory load = %v", err)
	}
	if _, _, err := store.PagePayload(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page load = %v", err)
	}
	if _, _, err := store.Node(context.Background(), idValue[NodeID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed node load = %v", err)
	}
}

func TestBudgetAccountsRejectInvalidHierarchiesAndOverflow(t *testing.T) {
	if DefaultSessionBudgetLimits().ActiveScans != DefaultSessionActiveScans ||
		DefaultShareBudgetLimits().MemoryBytes != DefaultShareCatalogMemory ||
		DefaultProcessBudgetLimits().Entries != DefaultProcessCommittedEntries {
		t.Fatal("catalog default budget profile drifted from the frozen limits")
	}
	if _, err := NewBudgetAccount("", BudgetLimits{ActiveScans: 1, ScanWork: 1, Entries: 1, MemoryBytes: 1}); err == nil {
		t.Fatal("unnamed budget was accepted")
	}
	if _, err := NewBudgetAccount("zero", BudgetLimits{}); err == nil {
		t.Fatal("zero budget was accepted")
	}
	process := generousBudget(t, "process")
	share := generousBudget(t, "share")
	session := generousBudget(t, "session")
	if _, err := ReserveHierarchy(BudgetHierarchy{}, ResourceUsage{Entries: 1}); err == nil {
		t.Fatal("nil hierarchy was accepted")
	}
	if _, err := ReserveHierarchy(BudgetHierarchy{Process: process, Share: process, Session: session}, ResourceUsage{Entries: 1}); err == nil {
		t.Fatal("aliased hierarchy was accepted")
	}
	if (*BudgetAccount)(nil).Name() != "" || (*BudgetAccount)(nil).Snapshot() != (BudgetSnapshot{}) {
		t.Fatal("nil budget inspection is not safe")
	}
	var nilReservation *BudgetReservation
	nilReservation.Release()
	if process.Name() != "process" || share.Name() != "share" {
		t.Fatal("budget names changed")
	}
}

func TestDirectorySnapshotRejectsCrossPageIdentityAndChainMutations(t *testing.T) {
	instance := idValue[ShareInstance](1)
	directory := idValue[DirectoryID](2)
	generation := idValue[DirectoryGeneration](3)
	first, _, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		Entries: testEntries(t, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: testEntries(t, 1, 1), Terminal: true,
	}
	validSecond, _, _ := NewCatalogPage(secondSpec)
	mutations := []func(*CatalogPage){
		func(page *CatalogPage) { page.shareInstance = idValue[ShareInstance](9) },
		func(page *CatalogPage) { page.pageIndex = 2 },
		func(page *CatalogPage) {
			page.previous = mustPageCommitment(t, bytes.Repeat([]byte{9}, PageCommitmentBytes))
		},
		func(page *CatalogPage) { page.terminal = false },
	}
	for index, mutate := range mutations {
		second := validSecond
		mutate(&second)
		if _, err := NewDirectorySnapshot([]CatalogPage{first, second}); err == nil {
			t.Fatalf("snapshot mutation %d was accepted", index)
		}
	}
	earlyTerminal := first
	earlyTerminal.terminal = true
	if _, err := NewDirectorySnapshot([]CatalogPage{earlyTerminal, validSecond}); err == nil {
		t.Fatal("early terminal marker was accepted")
	}
}
