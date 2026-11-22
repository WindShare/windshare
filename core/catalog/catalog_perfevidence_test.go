package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
)

var catalogWidths = [...]int{10_000, 100_000}

type wideCatalogMetrics struct {
	entries               uint64
	pages                 uint32
	peakSessionMemory     uint64
	retainedCatalogMemory uint64
}

func wideCatalogIdentity[T ~[IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func wideCatalogBudgets(tb testing.TB, prefix string) (*BudgetAccount, *BudgetAccount, *BudgetAccount, *BudgetAccount) {
	tb.Helper()
	newAccount := func(name string, accountLimits BudgetLimits) *BudgetAccount {
		account, err := NewBudgetAccount(prefix+"-"+name, accountLimits)
		if err != nil {
			tb.Fatal(err)
		}
		return account
	}
	process := newAccount("process", DefaultProcessBudgetLimits())
	share := newAccount("share", DefaultShareBudgetLimits())
	startup := newAccount("startup", DefaultSessionBudgetLimits())
	session := newAccount("session", DefaultSessionBudgetLimits())
	return process, share, startup, session
}

func wideCatalogStore(
	tb testing.TB,
	share ShareInstance,
	process, shareBudget *BudgetAccount,
) *CatalogStore {
	tb.Helper()
	var attempts atomic.Uint32
	var generations atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, ProcessBudget: process, ShareBudget: shareBudget,
		FailureEncoder: testFailureEncoder{},
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			id := wideCatalogIdentity[ScanAttemptID](79)
			binary.BigEndian.PutUint32(id[12:], attempts.Add(1))
			return id, nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			id := wideCatalogIdentity[DirectoryGeneration](97)
			binary.BigEndian.PutUint32(id[12:], generations.Add(1))
			return id, nil
		}),
	})
	if err != nil {
		tb.Fatal(err)
	}
	return store
}

func selectedWideDirectory(root, directory DirectoryID) (NodeRecord, error) {
	locator, err := NewSourceReference([]byte("object:" + ""))
	if err != nil {
		return NodeRecord{}, err
	}
	identity, err := NewSourceIdentity([]byte("wide-directory"))
	if err != nil {
		return NodeRecord{}, err
	}
	return NewDirectoryNodeRecord(directory, root, "wide", locator, identity, ModifiedTime{})
}

func benchmarkWideScannedFile(index int) (ScannedChild, error) {
	name := fmt.Sprintf("file-%07d", index)
	locator, err := NewSourceReference([]byte("object:" + name))
	if err != nil {
		return ScannedChild{}, err
	}
	identity, err := NewSourceIdentity(fmt.Appendf(nil, "source-%d", index))
	if err != nil {
		return ScannedChild{}, err
	}
	candidate, err := NewVersionCandidate(fmt.Appendf(nil, "version-%d", index))
	if err != nil {
		return ScannedChild{}, err
	}
	var file FileID
	binary.BigEndian.PutUint64(file[8:], uint64(index)+1)
	return ScannedChild{
		FileID: file, Name: name, SourceReference: locator, SourceIdentity: identity,
		VersionCandidate: candidate, ExpectedSize: uint64(index),
	}, nil
}

func pageObjectDigest(tb testing.TB, store *CatalogStore, directory CommittedDirectory) [sha256.Size]byte {
	tb.Helper()
	hash := sha256.New()
	var frame [12]byte
	for pageIndex := uint32(0); pageIndex < directory.PageCount(); pageIndex++ {
		object, found, err := store.PagePayload(context.Background(), directory.DirectoryID(), directory.Generation(), pageIndex)
		if err != nil || !found {
			tb.Fatalf("load page object %d: found=%v err=%v", pageIndex, found, err)
		}
		bytes := object.Bytes()
		binary.BigEndian.PutUint32(frame[0:4], pageIndex)
		binary.BigEndian.PutUint64(frame[4:12], uint64(len(bytes)))
		_, _ = hash.Write(frame[:])
		_, _ = hash.Write(bytes)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func exerciseCatalogWidth(tb testing.TB, width int) wideCatalogMetrics {
	tb.Helper()
	share := wideCatalogIdentity[ShareInstance](17)
	process, shareBudget, startup, session := wideCatalogBudgets(tb, "wide-catalog")
	store := wideCatalogStore(tb, share, process, shareBudget)
	defer func() {
		if err := store.Close(); err != nil {
			tb.Error(err)
		}
	}()
	rootDirectory := wideCatalogIdentity[DirectoryID](33)
	directory := wideCatalogIdentity[DirectoryID](51)
	selected, err := selectedWideDirectory(rootDirectory, directory)
	if err != nil {
		tb.Fatal(err)
	}
	rootCommit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: rootDirectory,
		Generation: wideCatalogIdentity[DirectoryGeneration](69), SelectedRoots: []NodeRecord{selected},
	})
	if err != nil {
		tb.Fatal(err)
	}
	if rootAuthority, err := store.CommitSyntheticRoot(context.Background(), rootCommit, startup); err != nil || rootAuthority.IsZero() {
		tb.Fatalf("commit synthetic root: authority=%v err=%v", !rootAuthority.IsZero(), err)
	}
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := width - 1; index >= 0; index-- {
			child, err := benchmarkWideScannedFile(index)
			if err != nil {
				return ScanResult{}, err
			}
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})
	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		tb.Fatal(err)
	}
	wantPages := uint32((width + MaxCatalogPageEntries - 1) / MaxCatalogPageEntries)
	if committed.EntryCount() != uint64(width) || committed.PageCount() != wantPages {
		tb.Fatalf("wide catalog geometry: entries=%d pages=%d", committed.EntryCount(), committed.PageCount())
	}
	sessionSnapshot := session.Snapshot()
	if sessionSnapshot.Used != (ResourceUsage{}) {
		tb.Fatalf("completed catalog scan retained session budget: %+v", sessionSnapshot.Used)
	}
	firstPage, found, err := store.Page(context.Background(), directory, committed.Generation(), 0)
	if err != nil || !found || len(firstPage.Entries()) == 0 || firstPage.Entries()[0].Name() != "file-0000000" {
		tb.Fatalf("first sorted page: found=%v err=%v", found, err)
	}
	lastPage, found, err := store.Page(context.Background(), directory, committed.Generation(), committed.PageCount()-1)
	wantLast := fmt.Sprintf("file-%07d", width-1)
	lastEntries := lastPage.Entries()
	if err != nil || !found || !lastPage.Terminal() || len(lastEntries) == 0 || lastEntries[len(lastEntries)-1].Name() != wantLast {
		tb.Fatalf("last sorted page: found=%v terminal=%v err=%v", found, lastPage.Terminal(), err)
	}
	firstDigest := pageObjectDigest(tb, store, committed)
	if secondDigest := pageObjectDigest(tb, store, committed); secondDigest != firstDigest {
		tb.Fatal("page payload replay changed committed bytes")
	}
	retained := shareBudget.Snapshot().Used
	if retained.Entries != uint64(width+1) || retained.MemoryBytes == 0 {
		tb.Fatalf("retained catalog budget = %+v", retained)
	}
	return wideCatalogMetrics{
		entries:               uint64(width),
		pages:                 committed.PageCount(),
		peakSessionMemory:     sessionSnapshot.Peak.MemoryBytes,
		retainedCatalogMemory: retained.MemoryBytes,
	}
}

func TestLongWideCatalogScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping wide-catalog scale test in -short mode")
	}
	metrics := exerciseCatalogWidth(t, 200_000)
	if metrics.entries != 200_000 || metrics.pages != 782 || metrics.retainedCatalogMemory == 0 {
		t.Fatalf("unexpected wide catalog metrics: %+v", metrics)
	}
}

func BenchmarkWideCatalog(b *testing.B) {
	for _, width := range catalogWidths {
		b.Run(fmt.Sprintf("entries=%07d", width), func(b *testing.B) {
			b.ReportAllocs()
			var last wideCatalogMetrics
			b.ResetTimer()
			for range b.N {
				last = exerciseCatalogWidth(b, width)
			}
			b.ReportMetric(float64(last.entries), "entries/op")
			b.ReportMetric(float64(last.pages), "pages/op")
			b.ReportMetric(float64(last.peakSessionMemory), "scan-peak-session-bytes")
			b.ReportMetric(float64(last.retainedCatalogMemory), "retained-catalog-bytes/op")
		})
	}
}
