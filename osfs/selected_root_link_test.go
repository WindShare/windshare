package osfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type linkedSelectionFixture struct {
	parent, target, link string
}

// newLinkedSelectionFixture builds target/{a.txt,inner/,note.txt} and link -> target.
func newLinkedSelectionFixture(t *testing.T) linkedSelectionFixture {
	t.Helper()
	parent := t.TempDir()
	fixture := linkedSelectionFixture{
		parent: parent, target: filepath.Join(parent, "target"), link: filepath.Join(parent, "link"),
	}
	if err := os.MkdirAll(filepath.Join(fixture.target, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.target, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.target, fixture.link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	return fixture
}

func openSelection(t *testing.T, selection string, omissions catalog.SourceOmissionObserver) (*SelectedFileSource, error) {
	t.Helper()
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths: []string{selection}, SyntheticRoot: catalog.DirectoryID{1}, Omissions: omissions,
	})
	if err == nil {
		t.Cleanup(func() { _ = source.Close() })
	}
	return source, err
}

// The selected root is the user's explicit choice, so a final link is followed
// as the kernel follows it, with or without a trailing separator. A shell entered
// through a link reports it in $PWD, which filepath.Abs returns for ".".
func TestSelectedRootFollowsAFinalLinkTheKernelFollows(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	chain := filepath.Join(fixture.parent, "chain")
	if err := os.Symlink(fixture.link, chain); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.link)
	separator := string(filepath.Separator)
	for name, test := range map[string]struct{ selection, root string }{
		"link":               {fixture.link, "link"},
		"dot in linked cwd":  {".", "link"},
		"trailing separator": {fixture.link + separator, "link"},
		"trailing dot":       {fixture.link + separator + ".", "link"},
		"parent of a child":  {"inner" + separator + "..", "link"},
		"two-level chain":    {chain, "chain"},
	} {
		t.Run(name, func(t *testing.T) {
			source, err := openSelection(t, test.selection, nil)
			if err != nil {
				t.Fatalf("selection %q = %v", test.selection, err)
			}
			root := source.SelectedRoots()[0]
			if _, directory := root.DirectoryID(); !directory || root.Entry().Name() != test.root {
				t.Fatalf("selected root = %q directory=%v, want the target under its typed name %q", root.Entry().Name(), directory, test.root)
			}
			children := &collectingScanChildren{}
			if _, err := source.ScanDirectory(context.Background(), catalog.ScanRequest{
				Directory: root, Work: &countingScanWork{}, Children: children,
			}); err != nil || len(children.items) != 2 {
				t.Fatalf("scan = %d children, %v", len(children.items), err)
			}
		})
	}
}

func TestSelectedFileLinkSharesItsTargetUnderTheTypedName(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	fileLink := filepath.Join(fixture.parent, "current.txt")
	if err := os.Symlink(filepath.Join(fixture.target, "a.txt"), fileLink); err != nil {
		t.Fatal(err)
	}
	source, err := openSelection(t, fileLink, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := source.SelectedRoots()[0]
	if _, file := root.FileID(); !file || root.Entry().Name() != "current.txt" {
		t.Fatalf("selected file link = %q file=%v", root.Entry().Name(), file)
	}
}

func TestSelectedLinkOmissionsKeepTheTypedPath(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	if err := os.Symlink("a.txt", filepath.Join(fixture.target, "inside-link")); err != nil {
		t.Fatal(err)
	}
	var reports []catalog.SourceOmissions
	source, err := openSelection(t, fixture.link, catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
		reports = append(reports, value)
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := source.ScanDirectory(t.Context(), catalog.ScanRequest{
		Directory: source.SelectedRoots()[0], Work: &countingScanWork{}, Children: &collectingScanChildren{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result.PublishOmissions(0, nil)
	if len(reports) != 1 || reports[0].DirectoryPath != fixture.link ||
		len(reports[0].Samples) != 1 || reports[0].Samples[0].Reason != catalog.SourceOmissionSymbolicLink {
		t.Fatalf("links inside the root must stay omitted and be reported under the typed path: %+v", reports)
	}
}

func TestSelectedLinkResolvingElsewhereStaysUnsupported(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	elsewhere := filepath.Join(fixture.parent, "elsewhere", "child")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(fixture.target, "jump")); err != nil {
		t.Fatal(err)
	}
	separator := string(filepath.Separator)
	for name, test := range map[string]struct {
		selection string
		failure   catalog.SourceFailure
		cause     error
	}{
		// Lexical cleaning yields "link", but the kernel resolves the typed path to "elsewhere".
		"lexical and kernel disagree": {fixture.link + separator + "jump" + separator + "..", catalog.SourceFailureUnsupported, content.ErrUnsupportedStability},
	} {
		t.Run(name, func(t *testing.T) {
			source, err := openSelection(t, test.selection, nil)
			selected, labelled := errors.AsType[*catalog.SelectionError](err)
			failure, classified := errors.AsType[*catalog.SourceError](err)
			if source != nil || !errors.Is(err, test.cause) || !labelled || selected.Label != test.selection ||
				!classified || failure.Failure != test.failure {
				t.Fatalf("selection %q = %v, %v", test.selection, source, err)
			}
		})
	}
}

// Linux fs.protected_symlinks answers EACCES for a link the kernel refuses to
// follow even though user-space EvalSymlinks would resolve it.
func TestSelectedLinkTheKernelRefusesToFollowReportsAccessDenied(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	refuse := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: fixture.link, Err: fs.ErrPermission}
	}
	_, err := resolveSelectedRoot(fixture.link, refuse)
	failure, classified := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, fs.ErrPermission) || !classified || failure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("kernel refusal = %v", err)
	}
	disagree := func(string) (fs.FileInfo, error) { return os.Stat(fixture.parent) }
	if _, err := resolveSelectedRoot(fixture.link, disagree); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("kernel and lexical resolution disagree = %v", err)
	}
}

func TestSelectedLinkRootMustOpenTheKernelResolvedObject(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	selection, err := resolveSelectedRoot(fixture.link, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !selection.openedFollowedTarget(opened) {
		t.Fatal("opened link target was not recognized")
	}
	// A link retargeted between resolution and open must not bind the new object.
	replacement, err := os.Open(fixture.parent)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if selection.openedFollowedTarget(replacement) {
		t.Fatal("a different opened object was accepted as the resolved link target")
	}
}
