package osfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
)

func openLinkedTestSource(
	t *testing.T,
	paths []string,
	sink diag.Sink,
	omissions catalog.SourceOmissionObserver,
) (*SelectedFileSource, error) {
	t.Helper()
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths:         paths,
		SyntheticRoot: catalog.DirectoryID{1},
		Diagnostics:   sink,
		Omissions:     omissions,
	})
	if err == nil {
		t.Cleanup(func() { _ = source.Close() })
	}
	return source, err
}

func assertSelectionFailure(
	t *testing.T,
	paths []string,
	wantLabel string,
	wantCause error,
	wantFailure catalog.SourceFailure,
) {
	t.Helper()
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths:         paths,
		SyntheticRoot: catalog.DirectoryID{1},
	})
	if source != nil {
		_ = source.Close()
		t.Fatalf("selection %q succeeded unexpectedly", paths)
	}
	selected, hasSelection := errors.AsType[*catalog.SelectionError](err)
	failure, hasSource := errors.AsType[*catalog.SourceError](err)
	if !errors.Is(err, wantCause) ||
		!hasSelection || selected.Label != wantLabel ||
		!hasSource || failure.Failure != wantFailure {
		t.Fatalf("selection %q = %v (selected=%+v, failure=%+v), want (%v, %v) on %q",
			paths, err, selected, failure, wantCause, wantFailure, wantLabel)
	}
}

func TestSelectedDirectorySymlinkSharesTargetAcrossPathForms(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-target")
	nested := filepath.Join(target, "sub", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("shared through directory symlink")
	if err := os.WriteFile(filepath.Join(target, "a.txt"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "b.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "my-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	chain := filepath.Join(base, "my-chain")
	if err := os.Symlink(link, chain); err != nil {
		t.Fatal(err)
	}
	relativeLink := filepath.Join(base, "rel-link")
	if err := os.Symlink("real-target", relativeLink); err != nil {
		t.Fatal(err)
	}
	// An inner symlink must stay omitted even when the selected root resolved
	// through a symlink.
	if err := os.Symlink("a.txt", filepath.Join(target, "inside-link")); err != nil {
		t.Fatal(err)
	}

	subViaLink := filepath.Join(link, "sub")
	nestedViaLink := filepath.Join(link, "sub", "nested")
	cases := []struct {
		name        string
		cwd         string
		selection   string
		wantRoot    string
		wantDisplay string
	}{
		{"dot", link, ".", "my-link", link},
		{"dot slash", link, "./", "my-link", link},
		{"parent from sub", subViaLink, "..", "my-link", link},
		{"parent slash from sub", subViaLink, "../", "my-link", link},
		{"dot slash parent from sub", subViaLink, "./..", "my-link", link},
		{"grandparent from nested", nestedViaLink, "../..", "my-link", link},
		{"named absolute link", base, link, "my-link", link},
		{"named link trailing slash", base, link + "/", "my-link", link},
		{"named link dot suffix", base, link + "/.", "my-link", link},
		{"named relative link", base, "my-link", "my-link", link},
		{"relative target symlink", base, relativeLink, "rel-link", relativeLink},
		{"symlink chain dot", chain, ".", "my-chain", chain},
		{"symlink chain named", base, chain, "my-chain", chain},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct {
				name, cwd, selection, wantRoot, wantDisplay string
			}{"dot backslash", link, `.\`, "my-link", link},
			struct {
				name, cwd, selection, wantRoot, wantDisplay string
			}{"parent backslash", subViaLink, `..\`, "my-link", link},
			struct {
				name, cwd, selection, wantRoot, wantDisplay string
			}{"grandparent backslash", nestedViaLink, `..\..`, "my-link", link},
			struct {
				name, cwd, selection, wantRoot, wantDisplay string
			}{"named backslash", base, link + `\`, "my-link", link},
			struct {
				name, cwd, selection, wantRoot, wantDisplay string
			}{"named backslash dot", base, link + `\.`, "my-link", link},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			stream, err := diag.NewStream(16, nil)
			if err != nil {
				t.Fatal(err)
			}
			var omissions []catalog.SourceOmissions
			source, err := openLinkedTestSource(t, []string{tc.selection}, stream.Sink(), catalog.SourceOmissionObserverFunc(func(summary catalog.SourceOmissions) {
				omissions = append(omissions, summary)
			}))
			if err != nil {
				t.Fatalf("open %q from %q failed: %v", tc.selection, tc.cwd, err)
			}
			root := source.SelectedRoots()[0]
			if _, ok := root.DirectoryID(); !ok || root.Entry().Name() != tc.wantRoot {
				t.Fatalf("root = (name=%q, dir=%v), want %q", root.Entry().Name(), ok, tc.wantRoot)
			}
			children := &collectingScanChildren{}
			result, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
				Directory: root,
				Work:      &countingScanWork{},
				Children:  children,
			})
			if err != nil {
				t.Fatalf("ScanDirectory failed: %v", err)
			}
			result.PublishOmissions(0, nil)
			stream.Close()

			if result.OmittedCount != 1 || len(children.items) != 2 {
				t.Fatalf("scan = (children=%d, omitted=%d), want (2, 1)", len(children.items), result.OmittedCount)
			}
			if len(omissions) != 1 || omissions[0].DirectoryPath != tc.wantDisplay ||
				len(omissions[0].Samples) != 1 || omissions[0].Samples[0].Reason != catalog.SourceOmissionSymbolicLink {
				t.Fatalf("omissions = %+v, want symbolic link omission under %q", omissions, tc.wantDisplay)
			}

			var fileChild catalog.ScannedChild
			foundFile := false
			for _, child := range children.items {
				if child.Name == "a.txt" {
					fileChild = child
					foundFile = true
				}
			}
			if !foundFile {
				t.Fatalf("missing a.txt in scanned children: %+v", children.items)
			}
			parentID, _ := root.DirectoryID()
			record, err := catalog.NewFileNodeRecord(
				fileChild.FileID, parentID, fileChild.Name, fileChild.SourceReference,
				fileChild.SourceIdentity, fileChild.VersionCandidate, fileChild.ExpectedSize, fileChild.ModifiedTime,
			)
			if err != nil {
				t.Fatal(err)
			}
			stable, err := source.OpenStable(context.Background(), record)
			if err != nil {
				t.Fatalf("OpenStable(a.txt) failed: %v", err)
			}
			buf := make([]byte, stable.ExactSize())
			_, readErr := stable.ReadAt(context.Background(), buf, 0)
			closeErr := stable.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(buf, payload) {
				t.Fatalf("read = (%q, %v, %v), want %q", buf, readErr, closeErr, payload)
			}

			foundDecision := false
			for rec := range stream.Records() {
				if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "resolved_selected_link" {
					foundDecision = true
				}
			}
			if !foundDecision {
				t.Fatal("missing catalog_source_policy resolved_selected_link diagnostic trace")
			}
		})
	}
}

func TestSelectedFileSymlinkSharesOnlyTargetFileAndSurvivesLinkRemoval(t *testing.T) {
	base := t.TempDir()
	targetDir := filepath.Join(base, "target-dir")
	linkDir := filepath.Join(base, "link-dir")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("shared through file symlink")
	targetFile := filepath.Join(targetDir, "real-file.bin")
	siblingFile := filepath.Join(targetDir, "sibling-secret.bin")
	if err := os.WriteFile(targetFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("must not be authorized"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "published.bin")
	if err := os.Symlink(targetFile, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	chain := filepath.Join(linkDir, "chained.bin")
	if err := os.Symlink(link, chain); err != nil {
		t.Fatal(err)
	}

	stream, err := diag.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := openLinkedTestSource(t, []string{chain}, stream.Sink(), nil)
	if err != nil {
		t.Fatalf("open file symlink chain failed: %v", err)
	}
	stream.Close()

	root := source.SelectedRoots()[0]
	if _, ok := root.FileID(); !ok || root.Entry().Name() != "chained.bin" {
		t.Fatalf("root = (name=%q, file=%v), want chained.bin", root.Entry().Name(), ok)
	}

	// Deleting the symlink after selection must not break the pinned root authority.
	if err := os.Remove(chain); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	stable, err := source.OpenStable(context.Background(), root)
	if err != nil {
		t.Fatalf("OpenStable after link removal failed: %v", err)
	}
	buf := make([]byte, stable.ExactSize())
	_, readErr := stable.ReadAt(context.Background(), buf, 0)
	closeErr := stable.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(buf, payload) {
		t.Fatalf("read = (%q, %v, %v), want %q", buf, readErr, closeErr, payload)
	}

	// The borrowed parent root must never authorize a sibling file in targetDir.
	siblingSource, err := openLinkedTestSource(t, []string{siblingFile}, diag.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if leaked, err := source.OpenStable(context.Background(), siblingSource.SelectedRoots()[0]); err == nil {
		_ = leaked.Close()
		t.Fatal("file symlink root authorized a sibling file in the target directory")
	} else if !errors.Is(err, content.ErrRevisionStale) {
		t.Fatalf("sibling rejection = %v, want ErrRevisionStale", err)
	}

	foundDecision := false
	for rec := range stream.Records() {
		if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "resolved_selected_link" {
			foundDecision = true
		}
	}
	if !foundDecision {
		t.Fatal("missing catalog_source_policy resolved_selected_link diagnostic trace for file symlink")
	}
}

func TestSelectedDanglingAndLoopingSymlinksFailCleanly(t *testing.T) {
	base := t.TempDir()
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "does-not-exist"), dangling); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	assertSelectionFailure(t, []string{dangling}, dangling, fs.ErrNotExist, catalog.SourceFailureMissing)

	loopA := filepath.Join(base, "loop-a")
	loopB := filepath.Join(base, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}
	assertSelectionFailure(t, []string{loopA}, loopA, content.ErrUnsupportedStability, catalog.SourceFailureUnsupported)
}

func TestParentTraversalFromNestedSymlinkSharesLogicalParentConsistently(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real-parent")
	elsewhere := filepath.Join(base, "elsewhere", "child")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realParent, "logical.txt"), []byte("logical"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "elsewhere", "physical.txt"), []byte("physical"), 0o600); err != nil {
		t.Fatal(err)
	}
	jump := filepath.Join(realParent, "jump")
	if err := os.Symlink(elsewhere, jump); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}

	t.Chdir(jump)
	source, err := openLinkedTestSource(t, []string{".."}, diag.Sink{}, nil)
	if err != nil {
		t.Fatalf("sharing .. from real-parent/jump failed: %v", err)
	}
	root := source.SelectedRoots()[0]
	if root.Entry().Name() != "real-parent" {
		t.Fatalf("root name = %q, want real-parent", root.Entry().Name())
	}
	children := &collectingScanChildren{}
	if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
		Directory: root, Work: &countingScanWork{}, Children: children,
	}); err != nil {
		t.Fatal(err)
	}
	if len(children.items) != 1 || children.items[0].Name != "logical.txt" {
		t.Fatalf("children = %+v, want only logical.txt", children.items)
	}
}
