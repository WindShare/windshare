package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
)

var errSelectedSymbolicLink = errors.New("osfs: selected path is a symbolic link")

// selectedRoot separates the user's spelling of a selection from the object the
// kernel resolves it to.
type selectedRoot struct {
	absolute string      // typed spelling: published name and displayed paths
	inspect  string      // link-free path whose leaf is inspected without following
	followed fs.FileInfo // kernel view of the typed selection when it resolves through a final link
}

// resolveSelectedRoot keeps the no-follow boundary for a path that names a link,
// but filepath.Abs can manufacture one: it reports "." as a shell's logical $PWD
// and drops a trailing "/" or "/.", after which POSIX resolves the final link.
// Only when the kernel's own no-follow view of the typed path is a directory
// (`.`, `..`, `link/`, `link/.`) is the final link of the absolute spelling
// followed, and only if the kernel and the lexical resolution agree. Kernel
// refusals of such a path keep their own cause.
func resolveSelectedRoot(selectedPath string, follow func(string) (fs.FileInfo, error)) (selectedRoot, error) {
	absolute, err := filepath.Abs(selectedPath)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("resolve selected root", selectedPath, err)
	}
	selection := selectedRoot{absolute: absolute, inspect: absolute}
	if leaf, err := os.Lstat(absolute); err != nil || leaf.Mode()&fs.ModeSymlink == 0 {
		return selection, nil
	}
	namesLink, err := typedPathNamesLink(selectedPath)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("inspect selected root", absolute, err)
	}
	// The absolute spelling of a named link is the link itself; otherwise the
	// typed path is followed so the kernel, not lexical cleaning, resolves "..".
	followPath := selectedPath
	if namesLink {
		followPath = absolute
	}
	followed, err := follow(followPath)
	if errors.Is(err, syscall.ELOOP) {
		// A link loop is not a usable file or directory: inspection refuses it
		// as an unsupported source, as before.
		return selection, nil
	}
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("follow selected root", absolute, err)
	}
	if namesLink {
		return namedLinkSelection(selection, followed)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("resolve selected root link", absolute, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("inspect selected root link target", absolute, err)
	}
	if !followed.IsDir() || !target.IsDir() || !os.SameFile(followed, target) {
		return selectedRoot{}, fmt.Errorf("osfs: selected root %q does not resolve to the directory the system opens: %w", absolute, content.ErrUnsupportedStability)
	}
	selection.inspect, selection.followed = resolved, followed
	return selection, nil
}

// typedPathNamesLink reports whether the kernel's no-follow view of the typed
// path is not a directory: the user named the link itself. POSIX resolves a
// final link for ".", "..", "link/" and "link/."; Windows Lstat resolves it only
// for a trailing separator, because "." and ".." are cleaned lexically there, so
// those spellings name the link on Windows. A trailing separator after a link
// to a non-directory (`file-link/`) fails differently per platform (ENOTDIR,
// ERROR_INVALID_NAME, ...), so it is recognised by the link before it.
func typedPathNamesLink(selectedPath string) (bool, error) {
	typed, err := os.Lstat(selectedPath)
	if err == nil {
		return !typed.IsDir(), nil
	}
	trimmed := selectedPath
	for len(trimmed) > 1 && os.IsPathSeparator(trimmed[len(trimmed)-1]) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == selectedPath {
		return false, err
	}
	if leaf, leafErr := os.Lstat(trimmed); leafErr == nil && leaf.Mode()&fs.ModeSymlink != 0 {
		return true, nil
	}
	return false, err
}

// namedLinkSelection keeps the no-follow boundary for a path naming a link (or
// a non-directory through one). A link to a file or directory is refused with
// its own reason so the user learns the target can be selected; anything else
// (such as a process-substitution pipe) is refused by inspection as before.
func namedLinkSelection(selection selectedRoot, followed fs.FileInfo) (selectedRoot, error) {
	if followed.IsDir() || followed.Mode().IsRegular() {
		return selectedRoot{}, fmt.Errorf("osfs: selected root %q: %w", selection.absolute, errSelectedSymbolicLink)
	}
	return selection, nil
}

// openedFollowedTarget closes the window between resolution and open: the
// opened root must still be the object the kernel resolved the selection to.
func (selection selectedRoot) openedFollowedTarget(handle *os.File) bool {
	if selection.followed == nil {
		return true
	}
	opened, err := handle.Stat()
	return err == nil && os.SameFile(selection.followed, opened)
}

func (source *SelectedCatalogSource) traceSelectedRootLink(slot RootSlot) {
	if !source.diagnostics.Enabled() {
		return
	}
	source.diagnostics.Emit(diag.New("catalog_source_policy", diag.Fields{
		"root_slot": diag.Uint(uint64(slot)),
		"decision":  diag.Text("followed_selected_root_link"),
	}))
}
