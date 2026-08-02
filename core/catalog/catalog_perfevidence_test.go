package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const catalogSortRunBytes = uint64(1 << 20)

var catalogWidths = [...]int{10_000, 100_000}

type performancePageSealer struct{}

func (performancePageSealer) Seal(input PageCommitInput) (SealedPageObject, error) {
	var encoded bytes.Buffer
	_, _ = encoded.Write(input.ShareInstance.Bytes())
	_, _ = encoded.Write(input.DirectoryID.Bytes())
	_, _ = encoded.Write(input.Generation.Bytes())
	var number [4]byte
	binary.BigEndian.PutUint32(number[:], input.PageIndex)
	_, _ = encoded.Write(number[:])
	_, _ = encoded.Write(input.Previous.Bytes())
	for _, entry := range input.Entries {
		_, _ = encoded.WriteString(entry.Name())
		_, _ = encoded.Write(entry.NodeID().Bytes())
	}
	if input.Terminal {
		_ = encoded.WriteByte(1)
	}
	return NewSealedPageObject(encoded.Bytes())
}

func (performancePageSealer) SealFailure(record DirectoryFailureRecord) (SealedFailureObject, error) {
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
	return NewSealedFailureObject(encoded.Bytes())
}

type spillProbe struct {
	delegate     *FileSpillFactory
	writtenBytes atomic.Uint64
	commits      atomic.Uint64
	liveObjects  atomic.Uint64
	peakObjects  atomic.Uint64
}

func newSpillProbe(root string) *spillProbe {
	return &spillProbe{delegate: NewFileSpillFactory(root)}
}

func (probe *spillProbe) Recover(ctx context.Context, share ShareInstance) error {
	return probe.delegate.Recover(ctx, share)
}

func (probe *spillProbe) Destroy(share ShareInstance) error {
	return probe.delegate.Destroy(share)
}

func (probe *spillProbe) NewWorkspace(ctx context.Context, request SpillRequest) (SpillWorkspace, error) {
	workspace, err := probe.delegate.NewWorkspace(ctx, request)
	if err != nil {
		return nil, err
	}
	return &spillWorkspace{delegate: workspace, probe: probe}, nil
}

func (probe *spillProbe) committedObject() {
	live := probe.liveObjects.Add(1)
	probe.commits.Add(1)
	for {
		peak := probe.peakObjects.Load()
		if live <= peak || probe.peakObjects.CompareAndSwap(peak, live) {
			return
		}
	}
}

type spillWorkspace struct {
	delegate SpillWorkspace
	probe    *spillProbe
}

func (workspace *spillWorkspace) Create(ctx context.Context) (SpillWriter, error) {
	writer, err := workspace.delegate.Create(ctx)
	if err != nil {
		return nil, err
	}
	return &spillWriter{delegate: writer, probe: workspace.probe}, nil
}

func (workspace *spillWorkspace) Close() error { return workspace.delegate.Close() }

type spillWriter struct {
	delegate SpillWriter
	probe    *spillProbe
}

func (writer *spillWriter) Write(data []byte) (int, error) {
	written, err := writer.delegate.Write(data)
	writer.probe.writtenBytes.Add(uint64(written))
	return written, err
}

func (writer *spillWriter) Commit() (SpillObject, error) {
	object, err := writer.delegate.Commit()
	if err != nil {
		return nil, err
	}
	writer.probe.committedObject()
	return &spillObject{delegate: object, probe: writer.probe}, nil
}

func (writer *spillWriter) Abort() error { return writer.delegate.Abort() }

type spillObject struct {
	delegate SpillObject
	probe    *spillProbe
	removed  atomic.Bool
}

func (object *spillObject) Open(ctx context.Context) (io.ReadCloser, error) {
	return object.delegate.Open(ctx)
}

func (object *spillObject) Size() uint64 { return object.delegate.Size() }

func (object *spillObject) Remove() error {
	if err := object.delegate.Remove(); err != nil {
		return err
	}
	if object.removed.CompareAndSwap(false, true) {
		object.probe.liveObjects.Add(^uint64(0))
	}
	return nil
}

type wideCatalogMetrics struct {
	entries             uint64
	pages               uint32
	sortBytesWritten    uint64
	sortObjectCommits   uint64
	peakSortObjects     uint64
	peakSessionMemory   uint64
	retainedCatalogDisk uint64
}

func wideCatalogIdentity[T ~[IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func wideCatalogBudgets(tb testing.TB, prefix string, width int) (*BudgetAccount, *BudgetAccount, *BudgetAccount) {
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

func wideCatalogStore(
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
		PageSealer: performancePageSealer{}, SpillFactory: spill, SortRunBytes: sortRunBytes,
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
		_ = backend.Close()
		tb.Fatal(err)
	}
	return store, backend
}

func selectedWideDirectory(root, directory DirectoryID) (NodeRecord, error) {
	locator, err := NewLocator(0, "")
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

func pageObjectDigest(tb testing.TB, store *CatalogStore, directory CommittedDirectory) [sha256.Size]byte {
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

func exerciseCatalogWidth(tb testing.TB, root string, width int, sortRunBytes uint64) wideCatalogMetrics {
	tb.Helper()
	share := wideCatalogIdentity[ShareInstance](17)
	process, shareBudget, session := wideCatalogBudgets(tb, "wide-catalog", width)
	spill := newSpillProbe(filepath.Join(root, "sort"))
	store, backend := wideCatalogStore(tb, root, share, process, shareBudget, spill, sortRunBytes)
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
	if rootAuthority, err := store.CommitSyntheticRoot(context.Background(), rootCommit, session); err != nil || rootAuthority.IsZero() {
		tb.Fatalf("commit synthetic root: authority=%v err=%v", !rootAuthority.IsZero(), err)
	}
	var peakSession atomic.Uint64
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := width - 1; index >= 0; index-- {
			child, err := benchmarkWideScannedFile(index)
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
	beforeDigest := pageObjectDigest(tb, store, committed)
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
	recoveredProcess, recoveredShare, recoveredSession := wideCatalogBudgets(tb, "wide-catalog-recovered", width)
	recovered, _ := wideCatalogStore(
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
	if afterDigest := pageObjectDigest(tb, recovered, replayed); afterDigest != beforeDigest {
		tb.Fatal("restart changed sealed page object bytes")
	}
	if recoveredShare.Snapshot().Used.Entries != uint64(width+1) ||
		recoveredShare.Snapshot().Used.SpillBytes != retainedCatalogDisk {
		tb.Fatalf("recovered catalog budget = %+v", recoveredShare.Snapshot().Used)
	}
	return wideCatalogMetrics{
		entries: uint64(width), pages: committed.PageCount(), sortBytesWritten: spill.writtenBytes.Load(),
		sortObjectCommits: spill.commits.Load(), peakSortObjects: spill.peakObjects.Load(),
		peakSessionMemory: peakSession.Load(), retainedCatalogDisk: retainedCatalogDisk,
	}
}

func BenchmarkExtremeWidthCatalogSpill(b *testing.B) {
	for _, width := range catalogWidths {
		b.Run(fmt.Sprintf("entries=%07d/run_bytes=%07d", width, catalogSortRunBytes), func(b *testing.B) {
			base := b.TempDir()
			b.ReportAllocs()
			var last wideCatalogMetrics
			b.ResetTimer()
			for iteration := range b.N {
				last = exerciseCatalogWidth(
					b, filepath.Join(base, fmt.Sprintf("iteration-%d", iteration)), width, catalogSortRunBytes,
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
