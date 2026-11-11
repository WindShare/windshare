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

func TestNonCollidingDirectoryFitsSinglePayloadSortBudget(t *testing.T) {
	const (
		childCount   = 32
		locatorBytes = 16 << 10
		spillLimit   = 768 << 10
	)
	// These opaque locators fit comfortably once, but a second full payload
	// stream would exceed the same budget before the directory could commit.
	hierarchy, meter := sorterBudget(t, 4<<20, spillLimit)
	defer meter.Close()
	probe := newSpillProbe(t.TempDir())
	workspace, err := probe.NewWorkspace(t.Context(), SpillRequest{
		ShareInstance: idValue[ShareInstance](1), AttemptID: idValue[ScanAttemptID](2),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	sorter, err := newDirectorySorter(idValue[DirectoryID](200), workspace, meter, hierarchy, DefaultSortRunMemoryBytes)
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
	if result.omittedCount != 0 || result.nodes.Count() != childCount {
		t.Fatalf("entries=%d omitted=%d", result.nodes.Count(), result.omittedCount)
	}
	iterator, err := result.nodes.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		node, ok, err := iterator.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, node.Entry().Name())
		if !bytes.Equal(node.SourceReference().Bytes(), locator.Bytes()) {
			t.Fatal("admission changed the source locator")
		}
	}
	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names=%v want=%v", got, want)
	}
	if err := result.nodes.Release(meter); err != nil {
		t.Fatal(err)
	}
	if written := probe.writtenBytes.Load(); written > spillLimit {
		t.Fatalf("healthy directory rewrote node payloads: %d bytes", written)
	}
	for _, account := range []*BudgetAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session} {
		if usage := account.Snapshot().Used; usage != (ResourceUsage{}) {
			t.Fatalf("admission retained %s resources: %+v", account.Name(), usage)
		}
	}
}

func TestSiblingAdmissionFiltersCanonicalDuplicatesInDisplayOrder(t *testing.T) {
	store := newTinyRunStore(t)
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

func TestSiblingExclusionFailuresReleaseCursorReservations(t *testing.T) {
	for name, exclusion := range map[string]sortRecord{
		"before-node": {key: []byte("a")},
		"after-nodes": {key: []byte("z")},
		"empty-name":  {},
		"payload":     {key: []byte("b"), payload: []byte("unexpected")},
	} {
		t.Run(name, func(t *testing.T) {
			hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
			defer meter.Close()
			node, err := scannedFile(t, 130, "b", 1).nodeRecord(idValue[DirectoryID](110))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := encodeNodeRecord(node)
			if err != nil {
				t.Fatal(err)
			}
			object := func(record sortRecord) *fakeSpillObject {
				var buffer bytes.Buffer
				if err := writeSortRecord(&buffer, record, meter); err != nil {
					t.Fatal(err)
				}
				return &fakeSpillObject{data: buffer.Bytes(), size: uint64(buffer.Len())}
			}
			source := admittedNodeSource{
				source: sortedNodeSource{
					object: object(sortRecord{key: []byte("b"), payload: encoded}),
					count:  1, hierarchy: hierarchy,
				},
				names: object(exclusion), omittedCount: 1,
			}
			iterator, failure := source.Open(t.Context())
			if failure == nil {
				for {
					_, ok, err := iterator.Next(t.Context())
					if err != nil || !ok {
						failure = err
						break
					}
				}
				if err := iterator.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if !errors.Is(failure, ErrCorruptCatalogStorage) {
				t.Fatalf("invalid exclusion err=%v", failure)
			}
			if err := source.Release(meter); err != nil {
				t.Fatal(err)
			}
			for _, account := range []*BudgetAccount{hierarchy.Process, hierarchy.Share, hierarchy.Session} {
				if usage := account.Snapshot().Used; usage != (ResourceUsage{}) {
					t.Fatalf("failed filter retained %s resources: %+v", account.Name(), usage)
				}
			}
		})
	}
}

func TestSiblingAdmissionRejectsIdentityReuseBeforePublishingOmissions(t *testing.T) {
	store := newTinyRunStore(t)
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
