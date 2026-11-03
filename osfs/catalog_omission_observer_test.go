package osfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestStableScanReportsBoundedOmissionsWithoutDiagnostics(t *testing.T) {
	root := t.TempDir()
	const omitted = catalog.MaxSourceOmissionSamples + 3
	for index := range omitted {
		name := fmt.Sprintf("omitted-%02d\u200b", index)
		if err := os.MkdirAll(filepath.Join(root, name, "unscanned-subtree"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readable.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reports []catalog.SourceOmissions
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: catalog.DirectoryID{1},
		Omissions: catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
			reports = append(reports, value)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	request := catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], AttemptID: catalog.ScanAttemptID{2},
		Generation: catalog.DirectoryGeneration{3}, Work: &countingScanWork{},
		Children: &collectingScanChildren{},
	}
	result, err := source.ScanDirectory(t.Context(), request)
	if err != nil || result.OmittedCount != omitted || len(reports) != 1 {
		t.Fatalf("result=%+v reports=%+v err=%v", result, reports, err)
	}
	report := reports[0]
	directory, _ := request.Directory.DirectoryID()
	if report.Directory != directory || report.Attempt != request.AttemptID || report.Generation != request.Generation ||
		report.DirectoryPath != root || report.Count != omitted || len(report.Samples) != catalog.MaxSourceOmissionSamples {
		t.Fatalf("omission report lost source identity or bounded accounting: %+v", report)
	}
	seen := map[string]bool{}
	for _, sample := range report.Samples {
		if sample.Reason != catalog.SourceOmissionName || !strings.Contains(sample.Detail, "U+200B") || seen[sample.Name] {
			t.Fatalf("invalid sample: %+v", sample)
		}
		if _, err := os.Stat(filepath.Join(report.DirectoryPath, sample.Name)); err != nil {
			t.Fatalf("sample cannot locate its omitted source: %v", err)
		}
		seen[sample.Name] = true
	}
	if request.Work.(*countingScanWork).units != omitted+1 {
		t.Fatal("scan traversed an omitted directory")
	}

	reports = nil
	request.Children = &mutatingScanChildren{root: root}
	if _, err := source.ScanDirectory(t.Context(), request); !errors.Is(err, catalog.ErrDirectoryStale) {
		t.Fatalf("mutated directory scan=%v", err)
	}
	if len(reports) != 0 {
		t.Fatal("failed stability check published a definitive omission summary")
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.ScanDirectory(cancelled, request); !errors.Is(err, context.Canceled) || len(reports) != 0 {
		t.Fatalf("cancelled scan published an omission summary: %v", err)
	}
}
