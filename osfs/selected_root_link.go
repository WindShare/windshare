package osfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/windshare/windshare/core/content"
	diag "github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
)

// selectedRoot separates the user's spelling of a selection from the object it
// names. The selected root itself is the user's explicit choice, so a final
// link is followed exactly as the kernel follows it (as for a download
// container); links inside a shared directory stay omitted.
type selectedRoot struct {
	absolute string      // typed spelling: published name and displayed paths
	inspect  string      // link-free path whose leaf is inspected without following
	followed fs.FileInfo // kernel view of the typed selection when it ends in a link
}

// resolveSelectedRoot accepts a final link only when the kernel follows the
// typed selection (respecting policies such as Linux fs.protected_symlinks) and
// the lexical resolution names the same object. filepath.Abs reports "." as a
// shell's logical $PWD, so this also covers `wind share .` in a directory
// entered through a link. Kernel refusals keep their own cause.
func resolveSelectedRoot(selectedPath string, follow func(string) (fs.FileInfo, error)) (selectedRoot, error) {
	absolute, err := filepath.Abs(selectedPath)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("resolve selected root", selectedPath, err)
	}
	selection := selectedRoot{absolute: absolute, inspect: absolute}
	if link, err := os.Lstat(absolute); err != nil || link.Mode()&fs.ModeSymlink == 0 {
		// Inspection reports missing and non-link roots exactly as before.
		return selection, nil
	}
	followed, err := follow(selectedPath)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("follow selected link", absolute, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("resolve selected link", absolute, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil {
		return selectedRoot{}, pathfailure.Filesystem("inspect selected link target", absolute, err)
	}
	if target.Mode()&fs.ModeSymlink != 0 || !os.SameFile(followed, target) {
		return selectedRoot{}, fmt.Errorf("osfs: selected link %q does not resolve to the object the system opens: %w", absolute, content.ErrUnsupportedStability)
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

func (source *SelectedCatalogSource) traceSelectedRootLink(slot RootSlot, kind sourceEntryKind) {
	if !source.diagnostics.Enabled() {
		return
	}
	target := "file"
	if kind == sourceEntryDirectory {
		target = "directory"
	}
	source.diagnostics.Emit(diag.New("catalog_source_policy", diag.Fields{
		"root_slot": diag.Uint(uint64(slot)),
		"decision":  diag.Text("followed_selected_link"),
		"target":    diag.Text(target),
	}))
}
