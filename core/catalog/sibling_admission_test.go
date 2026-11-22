package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func sorterTestHierarchy(t *testing.T, memoryBytes uint64) (BudgetHierarchy, *attemptResourceMeter) {
	t.Helper()
	newAccount := func(name string) *BudgetAccount {
		account, err := NewBudgetAccount(name, BudgetLimits{
			ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: memoryBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	hierarchy := BudgetHierarchy{
		Process: newAccount("sort-process"),
		Share:   newAccount("sort-share"),
		Session: newAccount("sort-session"),
	}
	meter, err := newAttemptResourceMeter(hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	return hierarchy, meter
}

func TestNonCollidingDirectoryFitsSinglePayloadMemoryBudget(t *testing.T) {
	const (
		childCount   = 32
		locatorBytes = 16 << 10
		memoryLimit  = 768 << 10
	)
	// These opaque locators fit comfortably once in memory, while double-charging
	// them across sort and admission would exceed the same budget.
	hierarchy, meter := sorterTestHierarchy(t, memoryLimit)
	sorter, err := newDirectorySorter(idValue[DirectoryID](200), meter)
	if err != nil {
		t.Fatal(err)
	}
	defer sorter.Close()
	locator, err := NewSourceReference([]byte(strings.Repeat("x", locatorBytes)))
	if err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"Report", "report", "資料", "café"}
	var want []string
	for index := childCount - 1; index >= 0; index-- {
		name := fmt.Sprintf("%s-%02d", prefixes[index%len(prefixes)], index)
		child := scannedFile(t, byte(index+1), name, uint64(index))
		child.SourceReference = locator
		if err := sorter.Add(t.Context(), child); err != nil {
			t.Fatal(err)
		}
		want = append(want, name)
	}
	slices.Sort(want)
	result, err := sorter.Finish(t.Context())
	if err != nil {
		t.Fatalf("healthy directory exceeded its single-payload budget: %v", err)
	}
	if result.omittedCount != 0 || len(result.records) != childCount {
		t.Fatalf("entries=%d omitted=%d", len(result.records), result.omittedCount)
	}
	got := make([]string, 0, len(result.records))
	for _, node := range result.records {
		got = append(got, node.Entry().Name())
		if !bytes.Equal(node.SourceReference().Bytes(), locator.Bytes()) {
			t.Fatal("admission changed the source locator")
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names=%v want=%v", got, want)
	}
	meter.Close()
	for _, account := range []*BudgetAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session} {
		if usage := account.Snapshot().Used; usage != (ResourceUsage{}) {
			t.Fatalf("admission retained %s resources: %+v", account.Name(), usage)
		}
	}
}

func TestSiblingAdmissionFiltersCanonicalDuplicatesInDisplayOrder(t *testing.T) {
	store := newCollisionTestStore(t)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 110, 112)
	names := []string{"z", "Z", "b", "B", "m", "A", "a", "Q", "q", "aa", "C", "d", "é", "e\u0301", "same", "same"}
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index, name := range names {
			if err := request.Children.Add(ctx, scannedFile(t, byte(130+index), name, 1)); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})
	committed, err := store.ListChildren(t.Context(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	if committed.EntryCount() != 4 || committed.OmittedCount() != 12 {
		t.Fatalf("committed=%+v", committed)
	}
	page, found, err := store.Page(t.Context(), directory, committed.Generation(), 0)
	if err != nil || !found {
		t.Fatalf("page found=%t err=%v", found, err)
	}
	var got []string
	for _, entry := range page.Entries() {
		got = append(got, entry.Name())
	}
	if want := []string{"C", "aa", "d", "m"}; !slices.Equal(got, want) {
		t.Fatalf("kept=%v want=%v", got, want)
	}
}

func TestDirectorySorterValidatesInputsAndReleasesOmittedMemory(t *testing.T) {
	var nilSorter *directorySorter
	nilSorter.Close()
	if _, err := newDirectorySorter(idValue[DirectoryID](110), nil); err == nil {
		t.Fatal("sorter accepted a nil resource meter")
	}
	hierarchy, meter := sorterTestHierarchy(t, 1<<20)
	defer meter.Close()
	sorter, err := newDirectorySorter(idValue[DirectoryID](110), meter)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sorter.Add(cancelled, scannedFile(t, 130, "a", 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Add = %v", err)
	}
	if err := sorter.Add(t.Context(), ScannedChild{}); err == nil {
		t.Fatal("sorter accepted an invalid child")
	}
	// Add two colliding children including one decomposed NFD spelling so extra
	// pre-NFC bytes and omitted node bytes are both released on Finish.
	for _, child := range []ScannedChild{
		scannedFile(t, 131, "caf\u00e9.txt", 1),
		scannedFile(t, 132, "cafe\u0301.txt", 1),
		scannedFile(t, 133, "kept.txt", 1),
	} {
		if err := sorter.Add(t.Context(), child); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sorter.Finish(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Finish = %v", err)
	}
	beforeFinish := hierarchy.Session.Snapshot().Used.MemoryBytes
	result, err := sorter.Finish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.records) != 1 || result.records[0].Entry().Name() != "kept.txt" || result.omittedCount != 2 {
		t.Fatalf("sort result = %+v", result)
	}
	afterFinish := hierarchy.Session.Snapshot().Used.MemoryBytes
	if afterFinish != result.records[0].EstimatedMemoryBytes() || afterFinish >= beforeFinish {
		t.Fatalf("memory accounting before=%d after=%d want=%d", beforeFinish, afterFinish, result.records[0].EstimatedMemoryBytes())
	}
}

func TestSiblingAdmissionRejectsIdentityReuseBeforePublishingOmissions(t *testing.T) {
	store := newCollisionTestStore(t)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 110, 112)
	published := false
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for _, child := range []ScannedChild{
			scannedFile(t, 130, "B", 1),
			scannedFile(t, 131, "b", 1),
			scannedFile(t, 130, "z", 1),
		} {
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{
			Omissions: ScanOmissionReporterFunc(func(uint64, []SourceOmission) { published = true }),
		}, nil
	})
	if _, err := store.ListChildren(t.Context(), directory, session, ScanOptions{}, scanner); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("identity reuse err=%v", err)
	}
	if published {
		t.Fatal("invalid identity was treated as a recoverable name omission")
	}
	if _, found, err := store.Directory(t.Context(), directory); err != nil || found {
		t.Fatalf("invalid directory published: found=%t err=%v", found, err)
	}
}

func TestDirectorySorterInPlaceAdmissionReleasesRawNamesAndAllCollidingBatches(t *testing.T) {
	t.Run("non-colliding NFD releases extra raw spelling bytes", func(t *testing.T) {
		hierarchy, meter := sorterTestHierarchy(t, 1<<20)
		defer meter.Close()
		sorter, err := newDirectorySorter(idValue[DirectoryID](115), meter)
		if err != nil {
			t.Fatal(err)
		}
		defer sorter.Close()
		if err := sorter.Add(t.Context(), scannedFile(t, 134, "cafe\u0301.txt", 1)); err != nil {
			t.Fatal(err)
		}
		before := hierarchy.Session.Snapshot().Used.MemoryBytes
		result, err := sorter.Finish(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.records) != 1 || result.records[0].Entry().Name() != "caf\u00e9.txt" || result.omittedCount != 0 {
			t.Fatalf("non-colliding NFD result = %+v", result)
		}
		after := hierarchy.Session.Snapshot().Used.MemoryBytes
		if after != result.records[0].EstimatedMemoryBytes() || after >= before {
			t.Fatalf("NFD raw bytes not released: before=%d after=%d want=%d", before, after, result.records[0].EstimatedMemoryBytes())
		}
	})

	t.Run("all-colliding batch returns empty slice and releases all bytes", func(t *testing.T) {
		hierarchy, meter := sorterTestHierarchy(t, 1<<20)
		defer meter.Close()
		sorter, err := newDirectorySorter(idValue[DirectoryID](116), meter)
		if err != nil {
			t.Fatal(err)
		}
		defer sorter.Close()
		for _, child := range []ScannedChild{
			scannedFile(t, 135, "caf\u00e9.txt", 1),
			scannedFile(t, 136, "cafe\u0301.txt", 1),
		} {
			if err := sorter.Add(t.Context(), child); err != nil {
				t.Fatal(err)
			}
		}
		result, err := sorter.Finish(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if result.records != nil || result.omittedCount != 2 || len(result.omittedSamples) != 2 {
			t.Fatalf("all-colliding result = %+v", result)
		}
		if !strings.Contains(result.omittedSamples[0].Detail, "U+00E9") || !strings.Contains(result.omittedSamples[0].Detail, "U+0065 U+0301") {
			t.Fatalf("collision detail lost differing scalars: %+v", result.omittedSamples)
		}
		if used := hierarchy.Session.Snapshot().Used.MemoryBytes; used != 0 {
			t.Fatalf("all-colliding batch retained memory: %d", used)
		}
	})
}
