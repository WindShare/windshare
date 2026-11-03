package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	diag "github.com/windshare/windshare/core/diagnostics"
)

func TestChildAccessErrorsIsolatePermissionDenialsAndPreserveOtherCauses(t *testing.T) {
	for _, operation := range []string{"lstat", "open"} {
		for _, cause := range []error{fs.ErrPermission, syscall.EACCES, syscall.EPERM} {
			t.Run(fmt.Sprintf("%s/%v", operation, cause), func(t *testing.T) {
				wrapped := &fs.PathError{Op: operation, Path: "private.txt", Err: cause}
				omission, err := classifyChildAccessError("private.txt", fmt.Errorf("source: %w", wrapped))
				if err != nil || omission.Name != "private.txt" || omission.Reason != catalog.SourceOmissionAccessDenied {
					t.Fatalf("permission denial = %+v, %v", omission, err)
				}
			})
		}
	}
	for _, cause := range []error{fs.ErrNotExist, syscall.EIO, syscall.EMFILE} {
		omission, err := classifyChildAccessError("changing.txt", cause)
		var scanError *catalog.ScanError
		if omission != (catalog.SourceOmission{}) || !errors.Is(err, cause) || !errors.As(err, &scanError) {
			t.Fatalf("non-permission failure was omitted or lost its cause: %+v, %v", omission, err)
		}
	}
}

func TestCatalogScanOmitsUnreadableChildrenAndKeepsReadableSiblings(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"readable.txt", "denied.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"readable-dir", "denied-dir"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"denied.txt", "denied-dir"} {
		denyCatalogRead(t, filepath.Join(root, name))
	}
	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	var reports []catalog.SourceOmissions
	source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
		Paths: []string{root}, SyntheticRoot: catalog.DirectoryID{1}, Diagnostics: stream.Sink(),
		Omissions: catalog.SourceOmissionObserverFunc(func(report catalog.SourceOmissions) {
			reports = append(reports, report)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	children := &collectingScanChildren{}
	request := catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], AttemptID: catalog.ScanAttemptID{2}, Generation: catalog.DirectoryGeneration{3},
		Work: &countingScanWork{}, Children: children,
	}
	result, err := source.ScanDirectory(t.Context(), request)
	stream.Close()
	if err != nil {
		t.Fatalf("one unreadable child failed the whole directory: %v", err)
	}
	names := map[string]bool{}
	for _, child := range children.items {
		names[child.Name] = true
	}
	if result.OmittedCount != 2 || len(names) != 2 || !names["readable.txt"] || !names["readable-dir"] {
		t.Fatalf("scan=%+v children=%v", result, names)
	}
	if len(reports) != 1 {
		t.Fatalf("permission summary count = %d", len(reports))
	}
	report := reports[0]
	directory, _ := request.Directory.DirectoryID()
	if report.Count != 2 || len(report.Samples) != 2 || report.DirectoryPath != root ||
		report.Directory != directory || report.Generation != request.Generation || report.Attempt != request.AttemptID {
		t.Fatalf("permission summary lost accounting or scan identity: %+v", report)
	}
	reported := map[string]bool{}
	for _, sample := range report.Samples {
		if sample.Reason != catalog.SourceOmissionAccessDenied || sample.Detail != "" {
			t.Fatalf("permission summary misclassified the source: %+v", sample)
		}
		reported[sample.Name] = true
	}
	if !reported["denied.txt"] || !reported["denied-dir"] {
		t.Fatalf("permission summary lost the denied paths: %+v", report)
	}
	traced := map[string]bool{}
	for record := range stream.Records() {
		if record.Name() == "catalog_source_omission" && record.Payload().Get("reason").Text() == "access_denied" {
			traced[record.Payload().Get("relative_path").Text()] = true
		}
	}
	if len(traced) != 2 || !traced["denied.txt"] || !traced["denied-dir"] {
		t.Fatalf("omission evidence lost the denied paths: %v", traced)
	}
}

func TestCatalogSelectionRejectsUnreadableExplicitRoots(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "selected")
			var err error
			if directory {
				err = os.Mkdir(path, 0o700)
			} else {
				err = os.WriteFile(path, []byte("content"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			denyCatalogRead(t, path)
			source, err := NewSelectedCatalogSource(SelectedCatalogSourceConfig{
				Paths: []string{path}, SyntheticRoot: catalog.DirectoryID{1},
			})
			if source != nil {
				_ = source.Close()
			}
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("explicit unreadable selection = %v", err)
			}
		})
	}
}
