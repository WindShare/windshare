package catalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func newTinyRunStore(t *testing.T) *CatalogStore {
	t.Helper()
	var attempt atomic.Uint32
	var generation atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1),
		Backend:       NewMemoryCatalogBackend(),
		ProcessBudget: generousBudget(t, "process"),
		ShareBudget:   generousBudget(t, "share"),
		PageSealer:    semanticTestCommitter{},
		SpillFactory:  NewFileSpillFactory(t.TempDir()),
		// Force every record to flush and merge through external spill runs so
		// multi-run portable-name collision groups are exercised deterministically.
		SortRunBytes: 1,
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			return idValue[ScanAttemptID](byte(attempt.Add(1))), nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			return idValue[DirectoryGeneration](byte(generation.Add(20))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestExternalSortWithholdsCollidingSiblingGroupsAcrossSpillRuns(t *testing.T) {
	store := newTinyRunStore(t)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 110, 112)

	children := []ScannedChild{
		scannedFile(t, 130, "unique.txt", 4),
		scannedFile(t, 131, "Readme.md", 5),
		scannedFile(t, 132, "Straße.txt", 6),
		scannedFile(t, 133, "caf\u00e9.txt", 7),
		scannedFile(t, 134, "DOCS", 8),
		scannedFile(t, 135, "README.md", 9),
		scannedFile(t, 136, "z-tail.txt", 10),
		scannedFile(t, 137, "STRASSE.txt", 11),
		scannedFile(t, 138, "cafe\u0301.txt", 12),
		scannedFile(t, 139, "Docs", 13),
		scannedFile(t, 140, "docs", 14),
	}

	var publishedCount uint64
	var publishedSamples []SourceOmission
	var reporterCalls int
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for _, child := range children {
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{
			OmittedCount: 1,
			Omissions: ScanOmissionReporterFunc(func(additionalCount uint64, additionalSamples []SourceOmission) {
				reporterCalls++
				publishedCount = additionalCount
				publishedSamples = slices.Clone(additionalSamples)
			}),
		}, nil
	})

	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatalf("ListChildren failed: %v", err)
	}
	// Nine children belong to colliding groups (2 + 2 + 2 + 3), plus one source-level omission.
	if committed.EntryCount() != 2 || committed.OmittedCount() != 10 {
		t.Fatalf("committed = %+v, want 2 entries and 10 omitted", committed)
	}
	if reporterCalls != 1 || publishedCount != 9 || len(publishedSamples) != 9 {
		t.Fatalf("calls=%d publishedCount=%d samples=%+v", reporterCalls, publishedCount, publishedSamples)
	}

	page, found, err := store.Page(context.Background(), directory, committed.Generation(), 0)
	if err != nil || !found {
		t.Fatalf("Page found=%t err=%v", found, err)
	}
	var kept []string
	for _, entry := range page.Entries() {
		kept = append(kept, entry.Name())
	}
	if want := []string{"unique.txt", "z-tail.txt"}; !slices.Equal(kept, want) {
		t.Fatalf("kept entries = %v, want %v", kept, want)
	}

	detailsByName := make(map[string]SourceOmission, len(publishedSamples))
	for _, sample := range publishedSamples {
		if sample.Reason != SourceOmissionNameCollision || sample.Detail == "" {
			t.Fatalf("invalid collision sample: %+v", sample)
		}
		detailsByName[sample.Name] = sample
	}
	if got := detailsByName["caf\u00e9.txt"].Detail; !strings.Contains(got, "U+00E9 vs U+0065 U+0301") {
		t.Fatalf("NFC collision detail = %q, want scalar disambiguation", got)
	}
	if got := detailsByName["cafe\u0301.txt"].Detail; !strings.Contains(got, "U+0065 U+0301 vs U+00E9") {
		t.Fatalf("NFD collision detail = %q, want scalar disambiguation", got)
	}
	if got := detailsByName["README.md"].Detail; got != `collides with sibling "Readme.md"` {
		t.Fatalf("case-fold collision detail = %q", got)
	}
}

func TestAllCollidingDirectoryCommitsEmptyTerminalPageWithOmittedCount(t *testing.T) {
	store := newTinyRunStore(t)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 110, 112)

	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index, name := range []string{"Report.pdf", "report.pdf"} {
			if err := request.Children.Add(ctx, scannedFile(t, byte(150+index), name, 1)); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})

	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatalf("ListChildren failed: %v", err)
	}
	if committed.EntryCount() != 0 || committed.OmittedCount() != 2 || committed.PageCount() != 1 {
		t.Fatalf("committed = %+v, want empty terminal page with 2 omitted", committed)
	}
}

func TestWideCollisionGroupBoundsSamplesAndSuppressesPublicationOnPageLimit(t *testing.T) {
	store := newTinyRunStore(t)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 110, 112)

	const groupSize = MaxSourceOmissionSamples + 5
	var samples []SourceOmission
	var count uint64
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := range groupSize {
			// Generate distinct 5-letter ASCII case variants sharing one folded key.
			variant := []byte("abcde")
			for bit := range len(variant) {
				if index&(1<<bit) != 0 {
					variant[bit] -= 'a' - 'A'
				}
			}
			if err := request.Children.Add(ctx, scannedFile(t, byte(160+index), string(variant), 1)); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{
			Omissions: ScanOmissionReporterFunc(func(additionalCount uint64, additionalSamples []SourceOmission) {
				count = additionalCount
				samples = slices.Clone(additionalSamples)
			}),
		}, nil
	})

	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	if committed.OmittedCount() != groupSize || count != groupSize || len(samples) != MaxSourceOmissionSamples {
		t.Fatalf("committed=%+v count=%d samples=%d", committed, count, len(samples))
	}

	overflowStore := newTinyRunStore(t)
	defer overflowStore.Close()
	overflowDirectory := prepareScannableDirectory(t, overflowStore, session, 185, 186)
	publishedOnFailure := false
	overflowScanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index, name := range []string{"Dup", "dup"} {
			if err := request.Children.Add(ctx, scannedFile(t, byte(190+index), name, 1)); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{
			OmittedCount: MaxDirectoryEntries - 1,
			Omissions: ScanOmissionReporterFunc(func(uint64, []SourceOmission) {
				publishedOnFailure = true
			}),
		}, nil
	})
	if _, err := overflowStore.ListChildren(context.Background(), overflowDirectory, session, ScanOptions{}, overflowScanner); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("overflow scan err = %v, want %v", err, ErrPageLimit)
	}
	if publishedOnFailure {
		t.Fatal("failed commit published provisional omissions")
	}

	// Exercise identical-spelling duplicate detail and long scalar truncation deterministically.
	dup := NewSiblingCollisionOmission("same.txt", "same.txt")
	if dup.Detail != `duplicate sibling "same.txt"` || !dup.Reason.RequiresDetail() {
		t.Fatalf("unexpected duplicate omission: %+v", dup)
	}
	longNFC := strings.Repeat("\u00e9", maxCollisionScalarPreview+2)
	longNFD := strings.Repeat("e\u0301", maxCollisionScalarPreview+2)
	truncated := NewSiblingCollisionOmission(longNFC, longNFD)
	if !strings.Contains(truncated.Detail, "...") {
		t.Fatalf("expected bounded scalar preview in %q", truncated.Detail)
	}
}
