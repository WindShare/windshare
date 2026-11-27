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
	if typed, err := os.Lstat(selectedPath); err != nil || !typed.IsDir() {
		if err != nil && !errors.Is(err, syscall.ENOTDIR) {
			return selectedRoot{}, pathfailure.Filesystem("inspect selected root", absolute, err)
		}
		// The typed path names the link itself (or a non-directory through it):
		// inspection refuses it exactly as any other selected link.
		return selection, nil
	}
	followed, err := follow(selectedPath)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("follow selected root", absolute, err)
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
