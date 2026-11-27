package osfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

// filepath.Abs reports "." as a shell's logical $PWD and drops a trailing "/" or
// "/.". Such spellings name the directory the kernel reaches, not the link.
func TestSelectionResolvingThroughAFinalLinkSharesTheDirectory(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	chain := filepath.Join(fixture.parent, "chain")
	if err := os.Symlink(fixture.link, chain); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.link)
	separator := string(filepath.Separator)
	for name, test := range map[string]struct{ selection, root string }{
		"dot in linked cwd":      {".", "link"},
		"trailing separator":     {fixture.link + separator, "link"},
		"trailing dot":           {fixture.link + separator + ".", "link"},
		"parent of a child":      {"inner" + separator + "..", "link"},
		"chain with a separator": {chain + separator, "chain"},
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

// A path naming a link keeps the documented no-follow boundary: a glob match, a
// script's "$HOME/link" or a desktop selection never reaches outside it. A link
// to a file or directory gets its own reason so the user can select the target.
func TestSelectionNamingALinkStaysRefused(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	chain := filepath.Join(fixture.parent, "chain")
	if err := os.Symlink(fixture.link, chain); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(fixture.parent, "current.txt")
	if err := os.Symlink(filepath.Join(fixture.target, "a.txt"), fileLink); err != nil {
		t.Fatal(err)
	}
	separator := string(filepath.Separator)
	for name, paths := range map[string][]string{
		"directory link":            {fixture.link},
		"chain":                     {chain},
		"file link":                 {fileLink},
		"file link with separator":  {fileLink + separator},
		"glob match among siblings": {filepath.Join(fixture.target, "a.txt"), fixture.link},
	} {
		t.Run(name, func(t *testing.T) {
			source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
				Paths: paths, SyntheticRoot: catalog.DirectoryID{1},
			})
			selected, labelled := errors.AsType[*catalog.SelectionError](err)
			failure, classified := errors.AsType[*catalog.SourceError](err)
			if source != nil || !errors.Is(err, errSelectedSymbolicLink) || !labelled || selected.Label != paths[len(paths)-1] ||
				!classified || failure.Failure != catalog.SourceFailureSymbolicLink {
				t.Fatalf("selection %q = %v, %v", paths, source, err)
			}
		})
	}
}

// A named link that cannot reach a file or directory keeps main's reasons: the
// kernel's own refusal, or the generic unsupported source for special files
// such as a process-substitution pipe.
func TestSelectionNamingAnUnusableLinkKeepsItsReason(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	dangling := filepath.Join(fixture.parent, "dangling")
	if err := os.Symlink(filepath.Join(fixture.parent, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := openSelection(t, dangling, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dangling link = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	device := filepath.Join(fixture.parent, "device")
	if err := os.Symlink(os.DevNull, device); err != nil {
		t.Fatal(err)
	}
	failure, _ := errors.AsType[*catalog.SourceError](func() error { _, err := openSelection(t, device, nil); return err }())
	if failure == nil || failure.Failure != catalog.SourceFailureUnsupported {
		t.Fatalf("link to a special file = %+v", failure)
	}
}

func TestSelectedLinkOmissionsKeepTheTypedPath(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	if err := os.Symlink("a.txt", filepath.Join(fixture.target, "inside-link")); err != nil {
		t.Fatal(err)
	}
	var reports []catalog.SourceOmissions
	source, err := openSelection(t, fixture.link+string(filepath.Separator), catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
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

func TestSelectionThroughAFinalLinkKeepsKernelRefusals(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	dangling := filepath.Join(fixture.parent, "dangling")
	if err := os.Symlink(filepath.Join(fixture.parent, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
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
		"dangling link with separator": {dangling + separator, catalog.SourceFailureMissing, fs.ErrNotExist},
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
func TestSelectionTheKernelRefusesToFollowReportsAccessDenied(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	selection := fixture.link + string(filepath.Separator)
	refuse := func(string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: selection, Err: fs.ErrPermission}
	}
	_, err := resolveSelectedRoot(selection, refuse)
	failure, classified := errors.AsType[*catalog.SourceError](classifySourceError("acquire selected root", catalog.SourceReference{}, err))
	if !errors.Is(err, fs.ErrPermission) || !classified || failure.Failure != catalog.SourceFailureAccessDenied {
		t.Fatalf("kernel refusal = %v", err)
	}
	disagree := func(string) (fs.FileInfo, error) { return os.Stat(fixture.parent) }
	if _, err := resolveSelectedRoot(selection, disagree); !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("kernel and lexical resolution disagree = %v", err)
	}
}

func TestSelectedLinkRootMustOpenTheKernelResolvedObject(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	selection, err := resolveSelectedRoot(fixture.link+string(filepath.Separator), os.Stat)
	if err != nil || selection.followed == nil {
		t.Fatalf("selection = %+v, %v", selection, err)
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
