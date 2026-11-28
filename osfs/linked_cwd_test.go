package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
)

func TestIsPureDotTraversal(t *testing.T) {
	allowed := []string{
		".",
		"./",
		".//",
		"./.",
		"././",
		"..",
		"../",
		"../..",
		"../../",
		"./..",
		"../.",
	}
	rejected := []string{
		"",
		"/",
		"//",
		"/.",
		"/..",
		"...",
		". ",
		" .",
		"link",
		"link/",
		"link/.",
		"link/./",
		"inner/..",
		"dir/link/.",
		"./link",
		"./link/.",
		"../link",
		"C:.",
		"C:..",
	}
	if runtime.GOOS == "windows" {
		allowed = append(allowed, `.\`, `..\`, `..\..`, `..\..\`, `.\..`, `..\.`, `../.\`)
		rejected = append(rejected, `\`, `\.`, `\..`, `link\`, `link\.`, `link\.\`, `inner\..`, `dir\link\.`)
	} else {
		rejected = append(rejected, `.\`, `..\`)
	}
	for _, path := range allowed {
		if !isPureDotTraversal(path) {
			t.Fatalf("isPureDotTraversal(%q) = false, want true", path)
		}
	}
	for _, path := range rejected {
		if isPureDotTraversal(path) {
			t.Fatalf("isPureDotTraversal(%q) = true, want false", path)
		}
	}
}

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

func assertUnsupportedSelection(t *testing.T, paths []string, wantLabel string) {
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
	if !errors.Is(err, content.ErrUnsupportedStability) ||
		!hasSelection || selected.Label != wantLabel ||
		!hasSource || failure.Failure != catalog.SourceFailureUnsupported {
		t.Fatalf("selection %q = %v (selected=%+v, failure=%+v), want unsupported on %q",
			paths, err, selected, failure, wantLabel)
	}
}

func TestDotTraversalThroughSymlinkedWorkingDirectorySharesTarget(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-target")
	nested := filepath.Join(target, "sub", "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("shared through linked cwd")
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
	// An inner symlink must stay omitted even when the working directory root
	// resolved through a symlink.
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
		{"symlink chain dot", chain, ".", "my-chain", chain},
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
				if rec.Name() == "catalog_source_policy" && rec.Payload().Get("decision").Text() == "resolved_linked_cwd" {
					foundDecision = true
				}
			}
			if !foundDecision {
				t.Fatal("missing catalog_source_policy resolved_linked_cwd diagnostic trace")
			}
		})
	}
}

func TestNamedSymlinkSelectionsStayRefusedIncludingDotSuffixes(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(filepath.Join(target, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(target, "a.txt")
	if err := os.WriteFile(realFile, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "zz")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	fileLink := filepath.Join(dir, "file-link")
	if err := os.Symlink(realFile, fileLink); err != nil {
		t.Fatal(err)
	}

	t.Chdir(link)
	cases := []struct {
		name  string
		paths []string
	}{
		{"named dir link", []string{link}},
		{"named dir link with slash", []string{link + "/"}},
		{"named dir link with dot", []string{link + "/."}},
		{"named dir link with dot slash", []string{link + "/./"}},
		{"child then parent spelling", []string{"inner/.."}},
		{"glob-like dir/* with link", []string{realFile, link}},
		{"glob-like dir/*/ with link", []string{filepath.Join(target, "inner") + "/", link + "/"}},
		{"glob-like dir/*/. with link", []string{filepath.Join(target, "inner") + "/.", link + "/."}},
		{"named file link", []string{fileLink}},
		{"named file link with dot", []string{fileLink + "/."}},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			struct {
				name  string
				paths []string
			}{"named dir link backslash", []string{link + `\`}},
			struct {
				name  string
				paths []string
			}{"named dir link backslash dot", []string{link + `\.`}},
			struct {
				name  string
				paths []string
			}{"named dir link backslash dot backslash", []string{link + `\.\`}},
			struct {
				name  string
				paths []string
			}{"child backslash parent spelling", []string{`inner\..`}},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertUnsupportedSelection(t, tc.paths, tc.paths[len(tc.paths)-1])
		})
	}
}

func TestParentTraversalFromRealParentThroughNestedJumpKeepsLogicalParent(t *testing.T) {
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
		t.Fatalf("sharing .. from realParent/jump failed: %v", err)
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
		t.Fatalf("children = %+v, want only logical.txt from logical parent", children.items)
	}
}
