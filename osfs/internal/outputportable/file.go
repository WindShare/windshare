package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type file struct {
	handle *os.File
	parent *directory
	name   string
}

func (f *file) Close() error {
	if f == nil || f.handle == nil {
		return nil
	}
	err := errors.Join(f.handle.Close(), f.parent.Close())
	f.handle = nil
	return err
}
func (f *file) ReadAt(p []byte, off int64) (int, error)  { return f.handle.ReadAt(p, off) }
func (f *file) WriteAt(p []byte, off int64) (int, error) { return f.handle.WriteAt(p, off) }
func (f *file) Sync() error                              { return f.handle.Sync() }
func (f *file) Size() (uint64, error) {
	info, err := f.handle.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, outputcap.ErrUnsafeNamespace
	}
	return uint64(info.Size()), nil
}
func (f *file) SameFile(other outputcap.FileIdentity) (bool, error) {
	r, ok := other.(*file)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	left, e1 := f.handle.Stat()
	right, e2 := r.handle.Stat()
	return e1 == nil && e2 == nil && os.SameFile(left, right), errors.Join(e1, e2)
}
func (f *file) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	info, err := f.handle.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != size {
		return false, nil
	}
	return !modified.Present() || info.ModTime().Equal(time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))), nil
}
func (f *file) SetModifiedTime(modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	t := time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))
	return setFileModifiedTime(f.handle, t)
}

// Mtime is excluded: writes after a saved checkpoint legitimately change it.
// The stamp is best-effort metadata evidence, never an assertion about content.
func (f *file) RecoveryMetadata() ([]byte, error) { return metadataStamp(f.handle) }

func (d *directory) openFile(name string, flags int, mode fs.FileMode) (*file, error) {
	if _, err := d.child(name); err != nil {
		return nil, err
	}
	before, err := d.root.Lstat(name)
	if err == nil && !before.Mode().IsRegular() {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	handle, err := openFileAt(d, name, flags, mode)
	if err != nil {
		return nil, mapCollision(err)
	}
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || before != nil && !os.SameFile(before, info) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err, handle.Close())
	}
	parent, err := d.Duplicate()
	if err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return &file{handle: handle, parent: parent.(*directory), name: name}, nil
}
func (d *directory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	mode := fs.FileMode(0o644)
	if private {
		mode = 0o600
	}
	f, err := d.openFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return nil, err
	}
	if err := f.handle.Truncate(size); err != nil {
		return nil, errors.Join(err, d.RemoveFile(name, f), f.Close())
	}
	return f, nil
}
func (d *directory) OpenObservedFile(name string, _ bool) (outputcap.ObservedFile, error) {
	return d.openFile(name, os.O_RDONLY, 0)
}
func (d *directory) OpenRecoveryDurabilityFile(name string, _ bool) (outputcap.RecoveryDurabilityFile, error) {
	return d.openFile(name, os.O_RDWR, 0)
}
func (d *directory) OpenMutableFile(name string, _ bool) (outputcap.MutableFile, error) {
	return d.openFile(name, os.O_RDWR, 0)
}
func (d *directory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	f, ok := expected.(*file)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	current, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.handle.Stat()
	if err != nil || !os.SameFile(current, info) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return d.root.Remove(name)
}
func (d *directory) LinkFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	f, ok := source.(*file)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if _, err := d.child(name); err != nil {
		return nil, err
	}
	if err := linkFile(f, d, name); err != nil {
		return nil, mapCollision(err)
	}
	return d.OpenObservedFile(name, true)
}
func (d *directory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	f, ok := source.(*file)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	if _, err := d.child(name); err != nil {
		return err
	}
	return renameFile(f, d, name, true)
}
func (d *directory) PublishFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.PublishNoReplaceOutcome, error) {
	f, ok := source.(*file)
	if !ok {
		return 0, outputcap.ErrUnsafeNamespace
	}
	if _, err := d.child(name); err != nil {
		return 0, err
	}
	return publicationResult(renameFile(f, d, name, false))
}

func publicationResult(err error) (outputcap.PublishNoReplaceOutcome, error) {
	if errors.Is(err, fs.ErrExist) {
		return outputcap.PublishNoReplaceCollision, nil
	}
	if errors.Is(err, outputcap.ErrOrdinaryOutputUnsupported) {
		return 0, err
	}
	if err != nil {
		return outputcap.PublishNoReplaceIndeterminate, err
	}
	return outputcap.PublishNoReplaceCommitted, nil
}
func (d *directory) CreateOrdinaryOutputStage(stage outputcap.Directory, name string, size uint64) error {
	f, err := stage.CreateFile(name, false, int64(size))
	if err != nil {
		return err
	}
	return f.Close()
}
func (d *directory) ReservePublicDirectoryNoReplace(name string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	created, err := d.CreateDirectory(name, false)
	if errors.Is(err, fs.ErrExist) {
		return nil, outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate, err
	}
	return created, outputcap.PublishNoReplaceCommitted, nil
}
func (d *directory) InstallDirectoryNoReplace(candidate outputcap.Directory, name string) (outputcap.Directory, error) {
	from, ok := candidate.(*directory)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if _, err := d.child(name); err != nil {
		return nil, err
	}
	if err := renameDirectory(from, d, name); err != nil {
		return nil, mapCollision(err)
	}
	return d.OpenDirectory(name, true)
}

type lock struct {
	f       *file
	release func() error
}

func (l *lock) File() outputcap.MutableFile { return l.f }
func (l *lock) Close() error {
	if l.f == nil {
		return nil
	}
	err := errors.Join(l.release(), l.f.Close())
	l.f = nil
	return err
}
func (d *directory) AcquireLock(name string, existingOnly bool) (outputcap.Lock, bool, error) {
	created := false
	f, err := d.openFile(name, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) && !existingOnly {
		f, err = d.openFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		created = err == nil
		if errors.Is(err, fs.ErrExist) {
			f, err = d.openFile(name, os.O_RDWR, 0)
		}
	}
	if err != nil {
		return nil, false, err
	}
	release, err := lockFile(f.handle)
	if err != nil {
		return nil, created, errors.Join(err, f.Close())
	}
	return &lock{f: f, release: release}, created, nil
}
