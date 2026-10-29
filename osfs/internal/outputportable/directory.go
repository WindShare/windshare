package outputportable

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type directory struct {
	root   *directoryRoot
	handle *os.File
	path   string
	info   os.FileInfo
}

func openDirectory(path string) (*directory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	handle, err := openDirectoryHandle(root)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	current, err := handle.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, handle.Close(), root.Close())
	}
	if err := root.Close(); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return &directory{root: &directoryRoot{path: path, info: current}, handle: handle, path: path, info: current}, nil
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || !filepath.IsLocal(name) {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}
func (d *directory) validate() error {
	if d == nil || d.root == nil || d.handle == nil {
		return fs.ErrClosed
	}
	current, err := os.Lstat(d.path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(d.info, current) {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}
func (d *directory) child(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	if err := d.validate(); err != nil {
		return "", err
	}
	return filepath.Join(d.path, name), nil
}
func (d *directory) Close() error {
	if d == nil || d.root == nil {
		return nil
	}
	err := d.handle.Close()
	d.handle = nil
	d.root = nil
	return err
}
func (d *directory) Duplicate() (outputcap.Directory, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return openDirectory(d.path)
}
func (d *directory) Sync() error {
	if err := d.validate(); err != nil {
		return err
	}
	return syncDirectory(d.handle)
}
func (d *directory) Names(limit int) ([]string, error) {
	batch, err := d.ReadNameBatch(limit)
	if err == nil && batch.HasMore {
		err = outputcap.ErrUnsafeNamespace
	}
	if err != nil {
		return nil, err
	}
	return batch.Names, nil
}

func (d *directory) ReadNameBatch(limit int) (outputcap.DirectoryNameBatch, error) {
	if limit < 0 {
		return outputcap.DirectoryNameBatch{}, outputcap.ErrUnsafeNamespace
	}
	if err := d.validate(); err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}
	f, err := d.root.Open(".")
	if err != nil {
		return outputcap.DirectoryNameBatch{}, err
	}
	names, err := f.Readdirnames(limit + 1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return outputcap.DirectoryNameBatch{
		Names: names[:min(limit, len(names))], HasMore: len(names) > limit,
	}, errors.Join(err, f.Close())
}
func kind(info os.FileInfo) outputcap.EntryKind {
	if info == nil {
		return outputcap.EntryAbsent
	}
	if info.IsDir() {
		return outputcap.EntryDirectory
	}
	if info.Mode().IsRegular() {
		return outputcap.EntryRegularFile
	}
	return outputcap.EntryOther
}
func (d *directory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if _, err := d.child(name); err != nil {
		return 0, err
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return outputcap.EntryAbsent, nil
	}
	return kind(info), err
}
func (d *directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	k, err := d.ObserveEntry(name)
	if err != nil || k == outputcap.EntryAbsent || k == outputcap.EntryOther {
		return k, err == nil, err
	}
	// Reusing a directory through a case or short-name alias would merge source
	// trees. Ask the opened object for its spelling instead of guessing the
	// parent filesystem's Unicode equivalence relation.
	actual, err := entrySpelling(d, name)
	return k, err == nil && exactEntrySpelling(name, actual), err
}

type entry struct {
	file *os.File
	info os.FileInfo
	name string
}

func (e *entry) Kind() outputcap.EntryKind { return kind(e.info) }
func (e *entry) Close() error              { return e.file.Close() }
func (d *directory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if _, err := d.child(name); err != nil {
		return nil, err
	}
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, outputcap.ErrUnsafeNamespace
	}
	f, err := openFileAt(d, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, f.Close())
	}
	return &entry{file: f, info: current, name: name}, nil
}
func (d *directory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	e, ok := expected.(*entry)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil && os.SameFile(info, e.info), err
}
func (d *directory) OpenPinnedDirectory(expected outputcap.CurrentEntryReference, private bool) (outputcap.Directory, error) {
	e, ok := expected.(*entry)
	if !ok || !e.info.IsDir() {
		return nil, outputcap.ErrUnsafeNamespace
	}
	matched, err := d.EntryMatches(e.name, e)
	if err != nil || !matched {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return d.OpenDirectory(e.name, private)
}
func (d *directory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	matched, err := d.EntryMatches(name, expected)
	if err != nil || !matched {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return d.root.Remove(name)
}
func (d *directory) SameDirectory(other outputcap.Directory) (bool, error) {
	r, ok := other.(*directory)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	return os.SameFile(d.info, r.info), nil
}
func (d *directory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	path, err := d.child(name)
	if err != nil {
		return nil, err
	}
	return openDirectory(path)
}
func (d *directory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	path, err := d.child(name)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(0o755)
	if private {
		mode = 0o700
	}
	if err := d.root.Mkdir(name, mode); err != nil {
		return nil, mapCollision(err)
	}
	return openDirectory(path)
}
func (d *directory) RemoveDirectory(name string, expected outputcap.Directory) error {
	r, ok := expected.(*directory)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !os.SameFile(info, r.info) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return d.root.Remove(name)
}
func (d *directory) SetModifiedTime(modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	if err := d.validate(); err != nil {
		return err
	}
	t := time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))
	return setDirectoryTimes(d, t)
}
func mapCollision(err error) error {
	if errors.Is(err, fs.ErrExist) {
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	}
	return err
}
