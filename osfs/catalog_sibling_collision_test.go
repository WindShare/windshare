package osfs

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
)

type testCatalogSealer struct{}

func (testCatalogSealer) Commit(input catalog.PageCommitInput) (catalog.PageCommitment, error) {
	digest := sha256.Sum256(testPageBytes(input))
	return catalog.NewPageCommitment(digest[:])
}

func (testCatalogSealer) Seal(input catalog.PageCommitInput) (catalog.SealedPageObject, error) {
	return catalog.NewSealedPageObject(testPageBytes(input))
}

func (testCatalogSealer) SealFailure(record catalog.DirectoryFailureRecord) (catalog.SealedFailureObject, error) {
	payload := fmt.Appendf(nil, "%x:%x:%x:%d", record.ShareInstance(), record.DirectoryID(), record.AttemptID(), record.Kind())
	return catalog.NewSealedFailureObject(payload)
}

func testPageBytes(input catalog.PageCommitInput) []byte {
	payload := fmt.Appendf(
		nil, "%x:%x:%x:%d:%t:%d:%d",
		input.ShareInstance, input.DirectoryID, input.Generation,
		input.PageIndex, input.Terminal, input.OmittedCount, len(input.Entries),
	)
	for _, entry := range input.Entries {
		payload = fmt.Appendf(payload, "|%s:%x", entry.Name(), entry.NodeID())
	}
	return payload
}

func commitSourceDirectory(
	t *testing.T,
	source *SelectedCatalogSource,
	syntheticRoot catalog.DirectoryID,
) (catalog.CommittedDirectory, []string) {
	t.Helper()
	share := catalog.ShareInstance{1}
	process, err := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	shareBudget, err := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	sessionBudget, err := catalog.NewBudgetAccount("session", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: share,
		Backend:       catalog.NewMemoryCatalogBackend(),
		ProcessBudget: process,
		ShareBudget:   shareBudget,
		PageSealer:    testCatalogSealer{},
		SpillFactory:  catalog.NewFileSpillFactory(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	selected := source.SelectedRoots()
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: share,
		SyntheticRoot: syntheticRoot,
		Generation:    catalog.DirectoryGeneration{9},
		SelectedRoots: selected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitSyntheticRoot(t.Context(), rootCommit, sessionBudget); err != nil {
		t.Fatal(err)
	}

	directoryID, ok := selected[0].DirectoryID()
	if !ok {
		t.Fatal("selected root is not a directory")
	}
	committed, err := store.ListChildren(t.Context(), directoryID, sessionBudget, catalog.ScanOptions{}, source)
	if err != nil {
		t.Fatalf("ListChildren failed: %v", err)
	}

	var kept []string
	for pageIndex := range committed.PageCount() {
		page, found, err := store.Page(t.Context(), directoryID, committed.Generation(), pageIndex)
		if err != nil || !found {
			t.Fatalf("Page(%d) found=%t err=%v", pageIndex, found, err)
		}
		for _, entry := range page.Entries() {
			kept = append(kept, entry.Name())
		}
	}
	return committed, kept
}

func requireCaseSensitiveTempDir(t *testing.T) string {
	t.Helper()
	probe := t.TempDir()
	for _, name := range []string{"case-probe", "CASE-PROBE"} {
		if err := os.WriteFile(filepath.Join(probe, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Skip("filesystem folds ASCII letter case in directory entries")
	}
	return t.TempDir()
}

// NTFS and APFS preserve Straße.txt alongside STRASSE.txt because 1-to-1 UTF-16
// case tables do not expand sharp s, whereas WindShare's portable path policy
// applies Unicode 15 full case folding. This exercises real filesystem sibling
// collision withholding on Windows, macOS, and Linux alike.
func TestCatalogStoreWithholdsFullCaseFoldSiblingsOnFilesystem(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Straße.txt", "STRASSE.txt", "unique.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 3 {
		t.Skipf("filesystem folded sharp s: entries=%d err=%v", len(entries), err)
	}

	stream, err := diag.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reports []catalog.SourceOmissions
	synthetic := catalog.DirectoryID{1}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: synthetic,
		Diagnostics:   stream.Sink(),
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	committed, kept := commitSourceDirectory(t, source, synthetic)
	stream.Close()

	if !slices.Equal(kept, []string{"unique.txt"}) || committed.OmittedCount() != 2 {
		t.Fatalf("kept=%v committed=%+v", kept, committed)
	}
	if len(reports) != 1 || reports[0].Count != 2 || reports[0].DirectoryPath != root {
		t.Fatalf("reports=%+v", reports)
	}
	for _, sample := range reports[0].Samples {
		if sample.Reason != catalog.SourceOmissionNameCollision || !strings.Contains(sample.Detail, "collides with sibling") {
			t.Fatalf("unexpected sample: %+v", sample)
		}
	}

	var omissionRecords, summaryRecords int
	for record := range stream.Records() {
		switch record.Name() {
		case "catalog_source_omission":
			omissionRecords++
			if record.Payload().Get("reason").Text() != "name_collision" {
				t.Fatalf("unexpected trace reason: %+v", record.Payload())
			}
		case "catalog_source_omission_summary":
			summaryRecords++
			if record.Payload().Get("omitted_entries").Uint() != 2 {
				t.Fatalf("unexpected summary: %+v", record.Payload())
			}
		}
	}
	if omissionRecords != 2 || summaryRecords != 1 {
		t.Fatalf("omissionRecords=%d summaryRecords=%d", omissionRecords, summaryRecords)
	}
}

func TestCatalogStoreWithholdsCollidingSiblingsWithoutShadowingFromOmittedSources(t *testing.T) {
	root := requireCaseSensitiveTempDir(t)
	for _, name := range []string{"Readme.md", "README.md", "unique.txt", "sym.txt", "open.txt", "OPEN.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Docs", "docs"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Neither symlinks nor unreadable siblings enter the catalog, so neither may
	// shadow a readable partner or lose its own omission classification.
	if err := os.Symlink("unique.txt", filepath.Join(root, "SYM.txt")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	denyCatalogRead(t, filepath.Join(root, "OPEN.txt"))

	var reports []catalog.SourceOmissions
	synthetic := catalog.DirectoryID{1}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: synthetic,
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	committed, kept := commitSourceDirectory(t, source, synthetic)
	if want := []string{"open.txt", "sym.txt", "unique.txt"}; !slices.Equal(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	// Six omitted: two colliding pairs (4) + one symlink (1) + one unreadable file (1).
	if committed.OmittedCount() != 6 || len(reports) != 1 || reports[0].Count != 6 {
		t.Fatalf("committed=%+v reports=%+v", committed, reports)
	}
	reasons := map[string]catalog.SourceOmissionReason{}
	for _, sample := range reports[0].Samples {
		reasons[sample.Name] = sample.Reason
	}
	for _, name := range []string{"Readme.md", "README.md", "Docs", "docs"} {
		if reasons[name] != catalog.SourceOmissionNameCollision {
			t.Fatalf("%q omitted as %q, want name_collision", name, reasons[name])
		}
	}
	if reasons["SYM.txt"] != catalog.SourceOmissionSymbolicLink {
		t.Fatalf("SYM.txt omitted as %q, want symbolic_link", reasons["SYM.txt"])
	}
	if reasons["OPEN.txt"] != catalog.SourceOmissionAccessDenied {
		t.Fatalf("OPEN.txt omitted as %q, want access_denied", reasons["OPEN.txt"])
	}
}

func TestCatalogStoreWithholdsComposedAndDecomposedSiblingsOnFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux preserves composed and decomposed spellings as distinct directory entries")
	}
	root := t.TempDir()
	for _, name := range []string{"caf\u00e9.txt", "cafe\u0301.txt", "plain.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var reports []catalog.SourceOmissions
	synthetic := catalog.DirectoryID{1}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: synthetic,
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	committed, kept := commitSourceDirectory(t, source, synthetic)
	if !slices.Equal(kept, []string{"plain.txt"}) || committed.OmittedCount() != 2 || len(reports) != 1 {
		t.Fatalf("kept=%v committed=%+v reports=%+v", kept, committed, reports)
	}
	for _, sample := range reports[0].Samples {
		if !strings.Contains(sample.Detail, "U+00E9") || !strings.Contains(sample.Detail, "U+0065 U+0301") {
			t.Fatalf("missing Unicode scalar disambiguation in %+v", sample)
		}
	}
}

func TestCatalogStoreCommitsNonCollidingDirectoryWithoutOmissions(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "Straße.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}

	var reports []catalog.SourceOmissions
	synthetic := catalog.DirectoryID{1}
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: synthetic,
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	committed, kept := commitSourceDirectory(t, source, synthetic)
	if committed.OmittedCount() != 0 || len(reports) != 0 {
		t.Fatalf("committed=%+v reports=%+v", committed, reports)
	}
	if want := []string{"Straße.txt", "a.txt", "b.txt", "dir"}; !slices.Equal(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
}

type mutatingScanWork struct {
	path    string
	mutated bool
}

func (work *mutatingScanWork) Consume(uint64) error {
	if work.mutated {
		return nil
	}
	work.mutated = true
	return os.WriteFile(work.path, []byte("late"), 0o600)
}

func TestCatalogScanReportsStaleWithoutOmissionsWhenDirectoryMutatesMidScan(t *testing.T) {
	root := t.TempDir()
	for index := range catalogEnumerationBatchSize + 2 {
		if err := os.WriteFile(filepath.Join(root, "f"+strconv.Itoa(index)+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "hidden\u200b.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stream, err := diag.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reports []catalog.SourceOmissions
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths:         []string{root},
		SyntheticRoot: catalog.DirectoryID{1},
		Diagnostics:   stream.Sink(),
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })

	_, err = source.ScanDirectory(t.Context(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0],
		Children:  &collectingScanChildren{},
		Work:      &mutatingScanWork{path: filepath.Join(root, "late-arrival.txt")},
	})
	stream.Close()
	if !errors.Is(err, catalog.ErrDirectoryStale) || len(reports) != 0 {
		t.Fatalf("scan err=%v reports=%+v, want stale and no omission report", err, reports)
	}
	for record := range stream.Records() {
		t.Fatalf("stale scan emitted premature diagnostic record: %s", record.Name())
	}
}
