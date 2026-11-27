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

// followingSpelling is "link/./": a final "." that resolves the link on every
// platform (Windows Lstat follows only with a trailing separator).
func followingSpelling(link string) string {
	separator := string(filepath.Separator)
	return link + separator + "." + separator
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

// filepath.Abs reports "." as a shell's logical $PWD and drops a final "/.".
// A final "." or ".." element names the directory the kernel reaches, not the
// link. Windows resolves the final link only for a trailing separator: "." and
// ".." are cleaned lexically and Lstat follows a final link only when the name
// ends in a separator (os/stat_windows.go), so the POSIX-only spellings name
// the link there and stay refused.
func TestSelectionResolvingThroughAFinalLinkSharesTheDirectory(t *testing.T) {
	fixture := newLinkedSelectionFixture(t)
	chain := filepath.Join(fixture.parent, "chain")
	if err := os.Symlink(fixture.link, chain); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(fixture.link, "inner")
	separator := string(filepath.Separator)
	for name, test := range map[string]struct {
		cwd, selection, root string
		posixOnly            bool
	}{
		"dot in linked cwd":           {fixture.link, ".", "link", true},
		"dot with a separator":        {fixture.link, "." + separator, "link", false},
		"parent from inside the link": {inside, "..", "link", true},
		"parent with a separator":     {inside, ".." + separator, "link", false},
		"trailing dot":                {fixture.link, fixture.link + separator + ".", "link", true},
		"trailing dot and separator":  {fixture.link, fixture.link + separator + "." + separator, "link", false},
		"parent of a child":           {fixture.link, "inner" + separator + "..", "link", true},
		"chain with a trailing dot":   {fixture.link, chain + separator + "." + separator, "chain", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(test.cwd)
			source, err := openSelection(t, test.selection, nil)
			if test.posixOnly && runtime.GOOS == "windows" {
				if source != nil || !errors.Is(err, errSelectedSymbolicLink) {
					t.Fatalf("selection %q = %v, %v; want the named link refused on Windows", test.selection, source, err)
				}
				return
			}
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

// A path naming a link keeps the documented no-follow boundary: a glob match
// (including `dir/*/`, which spells linked directories as `link/`), a script's
// "$HOME/link" or a desktop selection never reaches outside it. A link to a
// file or directory gets its own reason so the user can select the target.
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
		"directory link":                   {fixture.link},
		"directory link with separator":    {fixture.link + separator},
		"chain":                            {chain},
		"chain with separator":             {chain + separator},
		"directory glob with a link":       {filepath.Join(fixture.target, "inner") + separator, fixture.link + separator},
		"file link":                        {fileLink},
		"file link with separator":         {fileLink + separator},
		"file link with separator and dot": {fileLink + separator + "."},
		"file link with dot and separator": {fileLink + separator + "." + separator},
		"glob match among siblings":        {filepath.Join(fixture.target, "a.txt"), fixture.link},
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
	// A loop never reaches a file or directory: main's unsupported source, not
	// an unavailable one. (Windows reports loops with its own error; not covered.)
	loop := filepath.Join(fixture.parent, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	separator := string(filepath.Separator)
	for name, selection := range map[string]string{
		"loop": loop, "loop with separator": loop + separator, "loop with separator and dot": loop + separator + ".",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openSelection(t, selection, nil)
			failure, _ := errors.AsType[*catalog.SourceError](err)
			if !errors.Is(err, content.ErrUnsupportedStability) || failure == nil || failure.Failure != catalog.SourceFailureUnsupported {
				t.Fatalf("link loop %q = %v", selection, err)
			}
		})
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
	source, err := openSelection(t, followingSpelling(fixture.link), catalog.SourceOmissionObserverFunc(func(value catalog.SourceOmissions) {
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
	type refusal struct {
		selection string
		failure   catalog.SourceFailure
		cause     error
	}
	disagreement := refusal{fixture.link + separator + "jump" + separator + "..", catalog.SourceFailureUnsupported, content.ErrUnsupportedStability}
	if runtime.GOOS == "windows" {
		// Windows cleans ".." lexically, so the typed path names the link itself.
		disagreement.failure, disagreement.cause = catalog.SourceFailureSymbolicLink, errSelectedSymbolicLink
	}
	for name, test := range map[string]refusal{
		"dangling link with separator": {dangling + separator, catalog.SourceFailureMissing, fs.ErrNotExist},
		// Lexical cleaning yields "link", but the kernel resolves the typed path to "elsewhere".
		"lexical and kernel disagree": disagreement,
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
	selection := followingSpelling(fixture.link)
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
	selection, err := resolveSelectedRoot(followingSpelling(fixture.link), os.Stat)
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
