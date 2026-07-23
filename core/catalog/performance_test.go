package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"path/filepath"
	"sync/atomic"
	"testing"
)

const r8CatalogSortRunBytes = uint64(1 << 20)

var r8CatalogWidths = [...]int{10_000, 100_000}

type r8SpillProbe struct {
	delegate     *FileSpillFactory
	writtenBytes atomic.Uint64
	commits      atomic.Uint64
	liveObjects  atomic.Uint64
	peakObjects  atomic.Uint64
}

func newR8SpillProbe(root string) *r8SpillProbe {
	return &r8SpillProbe{delegate: NewFileSpillFactory(root)}
}

func (probe *r8SpillProbe) Recover(ctx context.Context, share ShareInstance) error {
	return probe.delegate.Recover(ctx, share)
}

func (probe *r8SpillProbe) Destroy(share ShareInstance) error {
	return probe.delegate.Destroy(share)
}

func (probe *r8SpillProbe) NewWorkspace(ctx context.Context, request SpillRequest) (SpillWorkspace, error) {
	workspace, err := probe.delegate.NewWorkspace(ctx, request)
	if err != nil {
		return nil, err
	}
	return &r8SpillWorkspace{delegate: workspace, probe: probe}, nil
}

func (probe *r8SpillProbe) committedObject() {
	live := probe.liveObjects.Add(1)
	probe.commits.Add(1)
	for {
		peak := probe.peakObjects.Load()
		if live <= peak || probe.peakObjects.CompareAndSwap(peak, live) {
			return
		}
	}
}

type r8SpillWorkspace struct {
	delegate SpillWorkspace
	probe    *r8SpillProbe
}

func (workspace *r8SpillWorkspace) Create(ctx context.Context) (SpillWriter, error) {
	writer, err := workspace.delegate.Create(ctx)
	if err != nil {
		return nil, err
	}
	return &r8SpillWriter{delegate: writer, probe: workspace.probe}, nil
}

func (workspace *r8SpillWorkspace) Close() error { return workspace.delegate.Close() }

type r8SpillWriter struct {
	delegate SpillWriter
	probe    *r8SpillProbe
}

func (writer *r8SpillWriter) Write(data []byte) (int, error) {
	written, err := writer.delegate.Write(data)
	writer.probe.writtenBytes.Add(uint64(written))
	return written, err
}

func (writer *r8SpillWriter) Commit() (SpillObject, error) {
	object, err := writer.delegate.Commit()
	if err != nil {
		return nil, err
	}
	writer.probe.committedObject()
	return &r8SpillObject{delegate: object, probe: writer.probe}, nil
}

func (writer *r8SpillWriter) Abort() error { return writer.delegate.Abort() }

type r8SpillObject struct {
	delegate SpillObject
	probe    *r8SpillProbe
	removed  atomic.Bool
}

func (object *r8SpillObject) Open(ctx context.Context) (io.ReadCloser, error) {
	return object.delegate.Open(ctx)
}

func (object *r8SpillObject) Size() uint64 { return object.delegate.Size() }

func (object *r8SpillObject) Remove() error {
	if err := object.delegate.Remove(); err != nil {
		return err
	}
	if object.removed.CompareAndSwap(false, true) {
		object.probe.liveObjects.Add(^uint64(0))
	}
	return nil
}

type r8CatalogMetrics struct {
	entries             uint64
	pages               uint32
	sortBytesWritten    uint64
	sortObjectCommits   uint64
	peakSortObjects     uint64
	peakSessionMemory   uint64
	retainedCatalogDisk uint64
}

func r8CatalogIdentity[T ~[IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func r8CatalogBudgets(tb testing.TB, prefix string, width int) (*BudgetAccount, *BudgetAccount, *BudgetAccount) {
	tb.Helper()
	limits := BudgetLimits{
		ActiveScans: 4,
		ScanWork:    uint64(width)*4 + 64,
		Entries:     uint64(width) + 64,
		MemoryBytes: 32 << 20,
		SpillBytes:  2 << 30,
	}
	newAccount := func(name string, accountLimits BudgetLimits) *BudgetAccount {
		account, err := NewBudgetAccount(prefix+"-"+name, accountLimits)
		if err != nil {
			tb.Fatal(err)
		}
		return account
	}
	process := newAccount("process", limits)
	share := newAccount("share", limits)
	sessionLimits := limits
	sessionLimits.MemoryBytes = 8 << 20
	session := newAccount("session", sessionLimits)
	return process, share, session
}

func r8CatalogStore(
	tb testing.TB,
	root string,
	share ShareInstance,
	process, shareBudget *BudgetAccount,
	spill SpillFactory,
	sortRunBytes uint64,
) (*CatalogStore, *FileCatalogBackend) {
	tb.Helper()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		tb.Fatal(err)
	}
	var attempts atomic.Uint32
	var generations atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, Backend: backend, ProcessBudget: process, ShareBudget: shareBudget,
		PageSealer: semanticTestCommitter{}, SpillFactory: spill, SortRunBytes: sortRunBytes,
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			id := r8CatalogIdentity[ScanAttemptID](79)
			binary.BigEndian.PutUint32(id[12:], attempts.Add(1))
			return id, nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			id := r8CatalogIdentity[DirectoryGeneration](97)
			binary.BigEndian.PutUint32(id[12:], generations.Add(1))
			return id, nil
		}),
	})
	if err != nil {
		_ = backend.Close()
		tb.Fatal(err)
	}
	return store, backend
}

func r8SelectedDirectory(root, directory DirectoryID) (NodeRecord, error) {
	locator, err := NewLocator(0, "")
	if err != nil {
		return NodeRecord{}, err
	}
	identity, err := NewSourceIdentity([]byte("r8-wide-directory"))
	if err != nil {
		return NodeRecord{}, err
	}
	return NewDirectoryNodeRecord(directory, root, "wide", locator, identity, ModifiedTime{})
}

func r8WideScannedFile(index int) (ScannedChild, error) {
	name := fmt.Sprintf("file-%07d", index)
	locator, err := NewLocator(0, name)
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
		FileID: file, Name: name, Locator: locator, SourceIdentity: identity,
		VersionCandidate: candidate, ExpectedSize: uint64(index),
	}, nil
}

func r8PageObjectDigest(tb testing.TB, store *CatalogStore, directory CommittedDirectory) [sha256.Size]byte {
	tb.Helper()
	hash := sha256.New()
	var frame [12]byte
	for pageIndex := uint32(0); pageIndex < directory.PageCount(); pageIndex++ {
		page, found, err := store.Page(context.Background(), directory.DirectoryID(), directory.Generation(), pageIndex)
		if err != nil || !found {
			tb.Fatalf("load page %d: found=%v err=%v", pageIndex, found, err)
		}
		object, found, err := store.PageObject(context.Background(), directory.DirectoryID(), directory.Generation(), pageIndex)
		if err != nil || !found || object.Commitment() != page.Commitment() {
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

func r8ExerciseCatalogWidth(tb testing.TB, root string, width int, sortRunBytes uint64) r8CatalogMetrics {
	tb.Helper()
	share := r8CatalogIdentity[ShareInstance](17)
	process, shareBudget, session := r8CatalogBudgets(tb, "r8", width)
	spill := newR8SpillProbe(filepath.Join(root, "sort"))
	store, backend := r8CatalogStore(tb, root, share, process, shareBudget, spill, sortRunBytes)
	defer func() {
		if err := store.Close(); err != nil {
			tb.Error(err)
		}
	}()
	rootDirectory := r8CatalogIdentity[DirectoryID](33)
	directory := r8CatalogIdentity[DirectoryID](51)
	selected, err := r8SelectedDirectory(rootDirectory, directory)
	if err != nil {
		tb.Fatal(err)
	}
	rootCommit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: rootDirectory,
		Generation: r8CatalogIdentity[DirectoryGeneration](69), SelectedRoots: []NodeRecord{selected},
	})
	if err != nil {
		tb.Fatal(err)
	}
	if rootAuthority, err := store.CommitSyntheticRoot(context.Background(), rootCommit, session); err != nil || rootAuthority.IsZero() {
		tb.Fatalf("commit synthetic root: authority=%v err=%v", !rootAuthority.IsZero(), err)
	}
	var peakSession atomic.Uint64
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := width - 1; index >= 0; index-- {
			child, err := r8WideScannedFile(index)
			if err != nil {
				return ScanResult{}, err
			}
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
			used := session.Snapshot().Used.MemoryBytes
			for {
				peak := peakSession.Load()
				if used <= peak || peakSession.CompareAndSwap(peak, used) {
					break
				}
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
	if session.Snapshot().Used != (ResourceUsage{}) {
		tb.Fatalf("completed catalog scan retained session budget: %+v", session.Snapshot().Used)
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
	beforeDigest := r8PageObjectDigest(tb, store, committed)
	if spill.commits.Load() < 3 {
		tb.Fatalf("width %d did not exercise external spill: commits=%d", width, spill.commits.Load())
	}
	maxMetadataObjects := uint64(4*bits.Len(uint(width)) + 8)
	if spill.peakObjects.Load() > maxMetadataObjects {
		tb.Fatalf("sort metadata grew with width: peak=%d limit=%d", spill.peakObjects.Load(), maxMetadataObjects)
	}
	retainedCatalogDisk := shareBudget.Snapshot().Used.SpillBytes
	if retainedCatalogDisk == 0 {
		tb.Fatal("file catalog retained no durable bytes")
	}

	// Closing only the backend models handle eviction. Reopening must re-admit
	// the durable catalog and replay byte-identical sealed page objects.
	if err := backend.Close(); err != nil {
		tb.Fatal(err)
	}
	recoveredProcess, recoveredShare, recoveredSession := r8CatalogBudgets(tb, "r8-recovered", width)
	recovered, _ := r8CatalogStore(
		tb, root, share, recoveredProcess, recoveredShare,
		NewFileSpillFactory(filepath.Join(root, "sort")), sortRunBytes,
	)
	defer func() {
		if err := recovered.Close(); err != nil {
			tb.Error(err)
		}
	}()
	if rootAuthority, err := recovered.CommitSyntheticRoot(context.Background(), rootCommit, recoveredSession); err != nil || rootAuthority.IsZero() {
		tb.Fatalf("recover synthetic root: authority=%v err=%v", !rootAuthority.IsZero(), err)
	}
	replayed, found, err := recovered.Directory(context.Background(), directory)
	if err != nil || !found || replayed != committed {
		tb.Fatalf("recovered directory: found=%v err=%v", found, err)
	}
	if afterDigest := r8PageObjectDigest(tb, recovered, replayed); afterDigest != beforeDigest {
		tb.Fatal("restart changed sealed page object bytes")
	}
	if recoveredShare.Snapshot().Used.Entries != uint64(width+1) ||
		recoveredShare.Snapshot().Used.SpillBytes != retainedCatalogDisk {
		tb.Fatalf("recovered catalog budget = %+v", recoveredShare.Snapshot().Used)
	}
	return r8CatalogMetrics{
		entries: uint64(width), pages: committed.PageCount(), sortBytesWritten: spill.writtenBytes.Load(),
		sortObjectCommits: spill.commits.Load(), peakSortObjects: spill.peakObjects.Load(),
		peakSessionMemory: peakSession.Load(), retainedCatalogDisk: retainedCatalogDisk,
	}
}

func TestR8ExtremeWidthCatalogSpillAndReplay(t *testing.T) {
	const semanticWidth = 10_003
	metrics := r8ExerciseCatalogWidth(t, t.TempDir(), semanticWidth, r8CatalogSortRunBytes)
	if metrics.entries != semanticWidth || metrics.pages < 2 || metrics.sortBytesWritten == 0 {
		t.Fatalf("extreme-width evidence = %+v", metrics)
	}
}

func BenchmarkR8ExtremeWidthCatalogSpill(b *testing.B) {
	for _, width := range r8CatalogWidths {
		b.Run(fmt.Sprintf("entries=%07d/run_bytes=%07d", width, r8CatalogSortRunBytes), func(b *testing.B) {
			base := b.TempDir()
			b.ReportAllocs()
			var last r8CatalogMetrics
			b.ResetTimer()
			for iteration := range b.N {
				last = r8ExerciseCatalogWidth(
					b, filepath.Join(base, fmt.Sprintf("iteration-%d", iteration)), width, r8CatalogSortRunBytes,
				)
			}
			b.ReportMetric(float64(last.entries), "entries/op")
			b.ReportMetric(float64(last.pages), "pages/op")
			b.ReportMetric(float64(last.sortBytesWritten), "sort-spill-written-bytes/op")
			b.ReportMetric(float64(last.sortObjectCommits), "sort-object-commits/op")
			b.ReportMetric(float64(last.peakSortObjects), "peak-sort-objects")
			b.ReportMetric(float64(last.peakSessionMemory), "scan-peak-session-bytes")
			b.ReportMetric(float64(last.retainedCatalogDisk), "retained-catalog-bytes/op")
		})
	}
}
