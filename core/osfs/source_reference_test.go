package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestFilesystemReferenceOwnsPathInterpretation(t *testing.T) {
	decomposed := "Cafe\u0301/report.bin"
	reference, err := NewSourceReference(3, decomposed)
	if err != nil {
		t.Fatal(err)
	}
	location, err := parseSourceReference(reference)
	if err != nil || location.RootSlot() != 3 || location.RelativePath() != decomposed {
		t.Fatalf("native source spelling = %+v, %v", location, err)
	}
	for _, path := range []string{"../sibling", "/absolute", "a\\b", "a/", "a//b", "C:/file", "bad\x00name", string([]byte{0xff}), strings.Repeat("a", catalog.MaxNameBytes+1), "a" + strings.Repeat("/a", catalog.MaxPathDepth), strings.Repeat("a/", catalog.MaxPathBytes)} {
		if _, err := NewSourceReference(0, path); err == nil {
			t.Fatalf("unsafe filesystem path accepted: %q", path)
		}
	}
	if _, err := NewSourceReference(catalog.MaxSelectedRoots, "file"); err == nil {
		t.Fatal("out-of-range root accepted")
	}
	for _, raw := range [][]byte{{1}, {0, 0, 0}, {1, 0xff, 0xff}, append([]byte{1, 0, 0}, []byte("../outside")...)} {
		opaque, _ := catalog.NewSourceReference(raw)
		if _, err := parseSourceReference(opaque); err == nil {
			t.Fatalf("malformed private filesystem reference accepted: %x", raw)
		}
	}
}

func TestSelectedFileSourceRejectsSiblingEvenWithValidNativeEvidence(t *testing.T) {
	directory := t.TempDir()
	selectedPath, siblingPath := filepath.Join(directory, "selected.bin"), filepath.Join(directory, "sibling.bin")
	for _, path := range []string{selectedPath, siblingPath} {
		if err := os.WriteFile(path, []byte("native bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	synthetic := catalog.DirectoryID{1}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{selectedPath}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sibling, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{Paths: []string{siblingPath}, SyntheticRoot: synthetic})
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	selected := source.SelectedRoots()[0]
	foreign := sibling.SelectedRoots()[0]
	file, _ := selected.FileID()
	forged, err := catalog.NewFileNodeRecord(file, synthetic, "sibling.bin", foreign.SourceReference(), foreign.SourceIdentity(), foreign.VersionCandidate(), foreign.Entry().ExpectedSize(), foreign.Entry().ModifiedTime())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), forged); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("selected leaf exposed its sibling: %v", err)
	}
	if _, err := source.RevisionContinuity(forged); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("continuity accepted out-of-scope reference: %v", err)
	}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{Directory: forged}); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("scan accepted out-of-scope reference: %v", err)
	}
	stable, err := source.OpenStable(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, stable.ExactSize())
	if n, err := stable.ReadAt(context.Background(), data, 0); err != nil || n != len(data) || !bytes.Equal(data, []byte("native bytes")) {
		t.Fatalf("native file read = %q, %v", data, err)
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.OpenStable(context.Background(), selected); !errors.Is(err, content.ErrRevisionStoreClosed) {
		t.Fatalf("closed source opened a handle: %v", err)
	}
}

func TestSelectedFileSourceReportsAcquisitionAndReferenceFailures(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSelectedFileSource(cancelled, SelectedCatalogSourceConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition = %v", err)
	}
	if _, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{filepath.Join(t.TempDir(), "missing")}, SyntheticRoot: catalog.DirectoryID{1}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing selection = %v", err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{t.TempDir()}, SyntheticRoot: catalog.DirectoryID{2}})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	root := source.SelectedRoots()[0]
	directory, _ := root.DirectoryID()
	foreignReference, _ := NewSourceReference(1, "")
	foreign, _ := catalog.NewDirectoryNodeRecord(directory, root.Parent(), "foreign", foreignReference, root.SourceIdentity(), root.Entry().ModifiedTime())
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{Directory: foreign}); !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("out-of-range selected authority = %v", err)
	}
	if err := (*SelectedFileSource)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cause   error
		failure catalog.SourceFailure
	}{{os.ErrPermission, catalog.SourceFailureAccessDenied}, {os.ErrNotExist, catalog.SourceFailureMissing}, {content.ErrUnsupportedStability, catalog.SourceFailureUnsupported}, {content.ErrRevisionStale, catalog.SourceFailureStale}, {errors.New("unavailable"), catalog.SourceFailureUnavailable}} {
		err := classifySourceError("open", root.SourceReference(), test.cause)
		var typed *catalog.SourceError
		if !errors.As(err, &typed) || !errors.Is(err, test.cause) || typed.Failure != test.failure || typed.Reference != root.SourceReference() || typed.Error() == "" {
			t.Fatalf("source failure classification = %v", err)
		}
	}
}

func TestSelectedFileSourcePreservesMissingObjectEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(path, []byte("selected bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{path}, SyntheticRoot: catalog.DirectoryID{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	record := source.SelectedRoots()[0]
	if _, err := source.RevisionContinuity(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = source.OpenStable(context.Background(), record)
	var typed *catalog.SourceError
	if !errors.As(err, &typed) || typed.Failure != catalog.SourceFailureMissing || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, content.ErrRevisionStale) || content.RevisionComparisonOf(err) != content.RevisionComparisonUnavailable {
		t.Fatalf("missing native object lost source or comparison evidence: %v", err)
	}
}

func TestSelectedFileSourceCancelsBetweenRootsAndDiscoversOnDemand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	_, err := NewSelectedFileSource(ctx, SelectedCatalogSourceConfig{Paths: []string{root, filepath.Join(root, "missing")}, SyntheticRoot: catalog.DirectoryID{1}, Identities: CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) { cancel(); return [catalog.IdentityBytes]byte{2}, nil })})
	if !errors.Is(err, context.Canceled) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled acquisition opened another root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "child.bin"), []byte("child"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{Paths: []string{root}, SyntheticRoot: catalog.DirectoryID{3}})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	children := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{Directory: source.SelectedRoots()[0], Work: &countingScanWork{}, Children: children}); err != nil {
		t.Fatal(err)
	}
	if len(children.items) != 1 || children.items[0].Name != "child.bin" {
		t.Fatalf("selected-source discovery = %+v", children.items)
	}
}
