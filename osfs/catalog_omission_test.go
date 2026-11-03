package osfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	const omitted = catalog.MaxSourceOmissionSamples + 2
	for index := range omitted {
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
	if samples != catalog.MaxSourceOmissionSamples || summaries != 1 {
		t.Fatalf("samples=%d summaries=%d", samples, summaries)
	}
}

func TestCatalogOmitsNamesOutsideThePathGrammarWithoutFailingSiblings(t *testing.T) {
	root := t.TempDir()
	// Directional marks and ZWNJ (required in Persian words such as "books") are
	// format characters that the shared name grammar rejects today.
	unrepresentable := []string{"\u200eIMG-0001.jpg", "\u06a9\u062a\u0627\u0628\u200c\u0647\u0627.docx"}
	if runtime.GOOS != "windows" {
		unrepresentable = append(unrepresentable, "12:30.log", `back\slash.txt`, "tab\tname.txt")
	}
	if runtime.GOOS == "linux" {
		unrepresentable = append(unrepresentable, "latin1-caf\xe9.txt")
	}
	for _, name := range append([]string{"readable.txt"}, unrepresentable...) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// On NTFS "dir:with-colon" is alternate data stream syntax, so do not create it there.
	if runtime.GOOS != "windows" {
		// The omitted directory is never opened, so its subtree counts as one item.
		if err := os.MkdirAll(filepath.Join(root, "dir:with-colon", "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
		unrepresentable = append(unrepresentable, "dir:with-colon")
	}
	stream, err := diag.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	synthetic, _ := catalog.DirectoryIDFromBytes(identity16(73))
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: synthetic, Diagnostics: stream.Sink(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	children := &collectingScanChildren{}
	result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], Work: &countingScanWork{}, Children: children,
	})
	stream.Close()
	if err != nil || result.OmittedCount != uint64(len(unrepresentable)) ||
		len(children.items) != 1 || children.items[0].Name != "readable.txt" {
		t.Fatalf("scan=%+v children=%+v err=%v", result, children.items, err)
	}
	reasons := 0
	for record := range stream.Records() {
		if record.Name() == "catalog_source_omission" {
			if reason := record.Payload().Get("reason").Text(); reason != "unrepresentable_name" {
				t.Fatalf("omission reason = %q", reason)
			}
			reasons++
		}
	}
	if reasons != len(unrepresentable) {
		t.Fatalf("omission samples = %d, want %d", reasons, len(unrepresentable))
	}
}
