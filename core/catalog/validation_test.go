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
		SenderPublicKey: bytes.Repeat([]byte{1}, SenderPublicKeySize), PathPolicy: PathPolicyV1,
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
	if _, err := NewLocator(MaxRootSlots, "file"); err == nil {
		t.Fatal("out-of-range root slot was accepted")
	}
	if _, err := NewLocator(0, "../file"); err == nil {
		t.Fatal("escaping locator was accepted")
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
	locator, _ := NewLocator(0, "file")
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

type failingPageCommitter struct{ empty bool }

func (c failingPageCommitter) Commit(PageCommitInput) (PageCommitment, error) {
	if c.empty {
		return PageCommitment{}, nil
	}
	return PageCommitment{}, errors.New("commit failure")
}

func TestSnapshotConstructorsRejectMalformedSemanticChains(t *testing.T) {
	if _, err := NewPageCommitment(make([]byte, PageCommitmentBytes-1)); err == nil {
		t.Fatal("short commitment was accepted")
	}
	base := CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1), DirectoryID: idValue[DirectoryID](2),
		Generation: idValue[DirectoryGeneration](3), Terminal: true,
	}
	if _, err := NewCatalogPage(CatalogPageSpec{}, semanticTestCommitter{}); err == nil {
		t.Fatal("identity-free page was accepted")
	}
	if _, err := NewCatalogPage(base, nil); err == nil {
		t.Fatal("page without a committer was accepted")
	}
	withPrevious := base
	withPrevious.Previous = mustPageCommitment(t, bytes.Repeat([]byte{1}, PageCommitmentBytes))
	if _, err := NewCatalogPage(withPrevious, semanticTestCommitter{}); err == nil {
		t.Fatal("page zero predecessor was accepted")
	}
	later := base
	later.PageIndex = 1
	if _, err := NewCatalogPage(later, semanticTestCommitter{}); err == nil {
		t.Fatal("later page without predecessor was accepted")
	}
	if _, err := NewCatalogPage(base, failingPageCommitter{}); err == nil {
		t.Fatal("committer failure was accepted")
	}
	if _, err := NewCatalogPage(base, failingPageCommitter{empty: true}); err == nil {
		t.Fatal("zero commitment was accepted")
	}
	nonterminalEmpty := base
	nonterminalEmpty.Terminal = false
	if _, err := NewCatalogPage(nonterminalEmpty, semanticTestCommitter{}); err == nil {
		t.Fatal("empty nonterminal page was accepted")
	}
	unsorted := base
	unsorted.Entries = testEntries(t, 0, 2)
	unsorted.Entries[0], unsorted.Entries[1] = unsorted.Entries[1], unsorted.Entries[0]
	if _, err := NewCatalogPage(unsorted, semanticTestCommitter{}); err == nil {
		t.Fatal("unsorted entries were accepted")
	}
	if _, err := NewDirectorySnapshot(nil); err == nil {
		t.Fatal("empty snapshot was accepted")
	}
	omittedOverflow := base
	omittedOverflow.OmittedCount = MaxDirectoryEntries + 1
	overflowPage, err := NewCatalogPage(omittedOverflow, semanticTestCommitter{})
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

func TestMemoryBackendRejectsInvalidTransactionLifecycles(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	meter, err := newAttemptResourceMeter(BudgetHierarchy{
		Process: generousBudget(t, "validation-process"),
		Share:   generousBudget(t, "validation-share"),
		Session: generousBudget(t, "validation-session"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer meter.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.BeginDirectory(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled begin error = %v", err)
	}
	if _, err := backend.BeginDirectory(context.Background(), DirectoryID{}, DirectoryGeneration{}, meter); err == nil {
		t.Fatal("identity-free transaction was accepted")
	}
	tx, err := backend.BeginDirectory(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.PutDirectory(NodeRecord{}); err == nil {
		t.Fatal("invalid directory was staged")
	}
	if err := tx.PutChild(NodeRecord{}); err == nil {
		t.Fatal("invalid child was staged")
	}
	if err := tx.PutPage(CatalogPage{}, SealedPageObject{}); err == nil {
		t.Fatal("invalid page was staged")
	}
	if _, err := tx.Prepare(context.Background()); err == nil {
		t.Fatal("incomplete transaction committed")
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := tx.PutDirectory(NodeRecord{}); err == nil {
		t.Fatal("finished transaction accepted more state")
	}
	if _, _, err := backend.LoadDirectory(cancelled, idValue[DirectoryID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled directory load = %v", err)
	}
	if _, _, err := backend.LoadPage(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page load = %v", err)
	}
	if _, _, err := backend.LoadNode(cancelled, idValue[NodeID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled node load = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if backend.directories != nil || backend.nodes != nil {
		t.Fatal("closed memory backend retained catalog state after its budget was released")
	}
	if _, err := backend.BeginDirectory(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed begin = %v", err)
	}
	if _, _, err := backend.LoadDirectory(context.Background(), idValue[DirectoryID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed directory load = %v", err)
	}
	if _, _, err := backend.LoadPage(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page load = %v", err)
	}
	if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed node load = %v", err)
	}
}

func TestBudgetAccountsRejectInvalidHierarchiesAndOverflow(t *testing.T) {
	if DefaultSessionBudgetLimits().ActiveScans != DefaultSessionActiveScans ||
		DefaultShareBudgetLimits().SpillBytes != DefaultShareCatalogSpill ||
		DefaultProcessBudgetLimits().Entries != DefaultProcessCommittedEntries {
		t.Fatal("catalog default budget profile drifted from the frozen limits")
	}
	if _, err := NewBudgetAccount("", BudgetLimits{ActiveScans: 1, ScanWork: 1, Entries: 1, MemoryBytes: 1, SpillBytes: 1}); err == nil {
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
	first, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		Entries: testEntries(t, 0, 1),
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: testEntries(t, 1, 1), Terminal: true,
	}
	validSecond, _ := NewCatalogPage(secondSpec, semanticTestCommitter{})
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
