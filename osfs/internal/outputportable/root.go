package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Go's Windows Root intentionally denies directory deletion while open. Scope
// it to each operation so other owned handles can rename or retire directories.
// The retained directory handle still binds every fresh Root to the same object.
type directoryRoot struct {
	path string
	info os.FileInfo
}

func (r *directoryRoot) open() (*os.Root, error) {
	root, err := os.OpenRoot(r.path)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(info, r.info) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, root.Close())
	}
	return root, nil
}
func (r *directoryRoot) Open(name string) (*os.File, error) { return r.OpenFile(name, os.O_RDONLY, 0) }
func (r *directoryRoot) OpenFile(name string, flags int, mode fs.FileMode) (*os.File, error) {
	root, err := r.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(name, flags, mode)
}
func (r *directoryRoot) Lstat(name string) (os.FileInfo, error) {
	root, err := r.open()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Lstat(name)
}
func (r *directoryRoot) Mkdir(name string, mode fs.FileMode) error {
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Mkdir(name, mode)
}
func (r *directoryRoot) Remove(name string) error {
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(name)
}
func (r *directoryRoot) Chtimes(name string, access, modified time.Time) error {
	root, err := r.open()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Chtimes(name, access, modified)
}
