package osfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
)

func TestCatalogOmissionsRetainBoundedPathsAndScanCorrelation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.bin"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	const omitted = maxCatalogOmissionSamples + 2
	for index := uint64(0); index < omitted; index++ {
		if err := os.Symlink("target.bin", filepath.Join(root, fmt.Sprintf("link-%02d", index))); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
	}
	stream, err := diag.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(70))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: synthetic, Diagnostics: stream.Sink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	attempt, _ := catalog.ScanAttemptIDFromBytes(identity16(71))
	generation, _ := catalog.DirectoryGenerationFromBytes(identity16(72))
	children := &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], AttemptID: attempt, Generation: generation,
		Work: &countingScanWork{}, Children: children,
	})
	stream.Close()
	if err != nil || result.OmittedCount != omitted || len(children.items) != 1 || children.items[0].Name != "target.bin" {
		t.Fatalf("scan=%+v children=%v err=%v", result, children.items, err)
	}
	var samples, summaries uint64
	for record := range stream.Records() {
		if record.Correlation().Get("scan_attempt_id").Text() != diag.Identity(attempt.Bytes()).Text() ||
			record.Correlation().Get("generation_id").Text() != diag.Identity(generation.Bytes()).Text() {
			t.Fatal("omission evidence lost its scan identity")
		}
		payload := record.Payload()
		switch record.Name() {
		case "catalog_source_omission":
			samples++
			if !strings.HasPrefix(payload.Get("relative_path").Text(), "link-") || payload.Get("reason").Text() != "symbolic_link" {
				t.Fatalf("missing concrete skipped path or reason: %+v", payload)
			}
		case "catalog_source_omission_summary":
			summaries++
			if payload.Get("omitted_entries").Uint() != omitted || payload.Get("unsampled_entries").Uint() != 2 {
				t.Fatalf("unaccounted omission samples: %+v", payload)
			}
		default:
			t.Fatalf("unexpected record %s", record.Name())
		}
	}
	if samples != maxCatalogOmissionSamples || summaries != 1 {
		t.Fatalf("samples=%d summaries=%d", samples, summaries)
	}
}
