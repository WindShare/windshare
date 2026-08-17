package outputruntime

import (
	"errors"
	"io/fs"
	"os"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type portableRuntimeFile struct {
	filesystem *portableRuntimeFilesystem
	info       os.FileInfo

	mu     sync.Mutex
	path   string
	closed bool
}

func newPortableRuntimeFile(
	filesystem *portableRuntimeFilesystem,
	path string,
	file *os.File,
) (*portableRuntimeFile, error) {
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil {
		return nil, errors.Join(statErr, closeErr)
	}
	return &portableRuntimeFile{
		filesystem: filesystem,
		info:       info,
		path:       path,
	}, nil
}

func (file *portableRuntimeFile) usable() error {
	if file == nil || file.filesystem == nil || file.info == nil {
		return fs.ErrClosed
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return fs.ErrClosed
	}
	return nil
}

func (file *portableRuntimeFile) currentPath() (string, error) {
	if err := file.usable(); err != nil {
		return "", err
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	path, err := file.filesystem.findObjectPath(file.info, file.path)
	if err != nil {
		return "", err
	}
	file.path = path
	return path, nil
}

func (file *portableRuntimeFile) fixedLinkSourcePath() (string, error) {
	if err := file.usable(); err != nil {
		return "", err
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	info, err := os.Stat(file.path)
	if err != nil || !os.SameFile(info, file.info) {
		return "", errors.Join(outputcap.ErrFixedLinkSourceChanged, err)
	}
	return file.path, nil
}

func (file *portableRuntimeFile) ReadAt(target []byte, offset int64) (int, error) {
	path, err := file.currentPath()
	if err != nil {
		return 0, err
	}
	handle, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	read, readErr := handle.ReadAt(target, offset)
	closeErr := handle.Close()
	if readErr != nil {
		return read, readErr
	}
	return read, closeErr
}

func (file *portableRuntimeFile) WriteAt(source []byte, offset int64) (int, error) {
	path, err := file.currentPath()
	if err != nil {
		return 0, err
	}
	handle, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	written, writeErr := handle.WriteAt(source, offset)
	closeErr := handle.Close()
	if writeErr != nil {
		return written, writeErr
	}
	return written, closeErr
}

func (file *portableRuntimeFile) Close() error {
	if file == nil {
		return nil
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	file.closed = true
	return nil
}

func (file *portableRuntimeFile) Sync() error {
	return file.usable()
}

func (file *portableRuntimeFile) Size() (uint64, error) {
	path, err := file.currentPath()
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < 0 {
		return 0, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	return uint64(info.Size()), nil
}

func (file *portableRuntimeFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	path, err := file.currentPath()
	if err != nil {
		return err
	}
	return portableRuntimeSetModifiedTime(path, modified)
}

func (file *portableRuntimeFile) MetadataMatches(
	size uint64,
	modified catalog.ModifiedTime,
) (bool, error) {
	path, err := file.currentPath()
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Size() < 0 || uint64(info.Size()) != size {
		return false, nil
	}
	if !modified.Present() {
		return true, nil
	}
	return portableRuntimeModifiedTimeMatches(info.ModTime(), modified), nil
}

func (file *portableRuntimeFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if err := file.usable(); err != nil {
		return false, err
	}
	if other == nil {
		return false, nil
	}
	candidate, ok := other.(*portableRuntimeFile)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	if err := candidate.usable(); err != nil {
		return false, err
	}
	return file.filesystem == candidate.filesystem && os.SameFile(file.info, candidate.info), nil
}

type portableRuntimeEntryReference struct {
	filesystem *portableRuntimeFilesystem
	path       string
	info       os.FileInfo
	kind       outputcap.EntryKind

	mu     sync.Mutex
	closed bool
}

func (reference *portableRuntimeEntryReference) Kind() outputcap.EntryKind {
	if reference == nil {
		return outputcap.EntryAbsent
	}
	return reference.kind
}

func (reference *portableRuntimeEntryReference) Close() error {
	if reference == nil {
		return nil
	}
	reference.mu.Lock()
	defer reference.mu.Unlock()
	reference.closed = true
	return nil
}

func (reference *portableRuntimeEntryReference) isClosed() bool {
	reference.mu.Lock()
	defer reference.mu.Unlock()
	return reference.closed
}

type portableRuntimeLock struct {
	filesystem *portableRuntimeFilesystem
	identity   uint64
	file       *portableRuntimeFile

	mu     sync.Mutex
	closed bool
}

func (lock *portableRuntimeLock) File() outputcap.MutableFile {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	return lock.file
}

func (lock *portableRuntimeLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	lock.filesystem.mu.Lock()
	delete(lock.filesystem.locks, lock.identity)
	lock.filesystem.mu.Unlock()
	return lock.file.Close()
}

func portableRuntimeOpenLockFile(path string, existingOnly bool) (*os.File, bool, error) {
	if existingOnly {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		return file, false, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return file, true, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, false, err
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0)
	return file, false, err
}
