package checkpointstore

import (
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type faultDirectory struct {
	outputcap.Directory
	duplicate       func() (outputcap.Directory, error)
	names           func(int) ([]string, error)
	openDirectory   func(string, bool) (outputcap.Directory, error)
	createDirectory func(string, bool) (outputcap.Directory, error)
	createFile      func(string, bool, int64) (outputcap.File, error)
	openFile        func(string, bool, bool) (outputcap.File, error)
	linkFile        func(outputcap.File, string) (outputcap.File, error)
	replaceFile     func(outputcap.File, string) error
	observeEntry    func(string) (outputcap.EntryKind, error)
	sync            func() error
}

func (directory *faultDirectory) Duplicate() (outputcap.Directory, error) {
	if directory.duplicate != nil {
		return directory.duplicate()
	}
	return directory.Directory.Duplicate()
}

func (directory *faultDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

func (directory *faultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.openDirectory != nil {
		return directory.openDirectory(name, private)
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *faultDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.createDirectory != nil {
		return directory.createDirectory(name, private)
	}
	return directory.Directory.CreateDirectory(name, private)
}

func (directory *faultDirectory) CreateFile(name string, private bool, size int64) (outputcap.File, error) {
	if directory.createFile != nil {
		return directory.createFile(name, private, size)
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *faultDirectory) OpenFile(name string, private, writable bool) (outputcap.File, error) {
	if directory.openFile != nil {
		return directory.openFile(name, private, writable)
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *faultDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	if directory.linkFile != nil {
		return directory.linkFile(source, name)
	}
	return directory.Directory.LinkFileNoReplace(source, name)
}

func (directory *faultDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	if directory.replaceFile != nil {
		return directory.replaceFile(source, name)
	}
	return directory.Directory.ReplacePrivateFile(source, name)
}

func (directory *faultDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory.observeEntry != nil {
		return directory.observeEntry(name)
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *faultDirectory) Sync() error {
	if directory.sync != nil {
		return directory.sync()
	}
	return directory.Directory.Sync()
}

type faultFile struct {
	outputcap.File
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)
}

func (file *faultFile) ReadAt(encoded []byte, offset int64) (int, error) {
	if file.readAt != nil {
		return file.readAt(encoded, offset)
	}
	return file.File.ReadAt(encoded, offset)
}

func (file *faultFile) WriteAt(encoded []byte, offset int64) (int, error) {
	return file.writeAt(encoded, offset)
}

func (file *faultFile) Close() error { return nil }

type closeTrackingFile struct {
	outputcap.File
	closes *int
}

func (file *closeTrackingFile) Close() error {
	*file.closes = *file.closes + 1
	return nil
}

type memoryDirectory struct {
	outputcap.Directory
	mu         sync.Mutex
	dirs       map[string]*memoryDirectory
	files      map[string]*memoryFileData
	locks      map[string]*memoryLock
	namesCalls int
	syncCalls  int
}

func newMemoryDirectory() *memoryDirectory {
	return &memoryDirectory{
		dirs:  make(map[string]*memoryDirectory),
		files: make(map[string]*memoryFileData),
		locks: make(map[string]*memoryLock),
	}
}

func (directory *memoryDirectory) dirsForTest(t *testing.T, name string) *memoryDirectory {
	t.Helper()
	directory.mu.Lock()
	defer directory.mu.Unlock()
	child := directory.dirs[name]
	if child == nil {
		t.Fatalf("missing memory directory %q", name)
	}
	return child
}

func directorySyncCalls(directory *memoryDirectory) int {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return directory.syncCalls
}

func (directory *memoryDirectory) Close() error { return nil }

func (directory *memoryDirectory) Duplicate() (outputcap.Directory, error) { return directory, nil }

func (directory *memoryDirectory) Sync() error {
	directory.mu.Lock()
	directory.syncCalls++
	directory.mu.Unlock()
	return nil
}

func (directory *memoryDirectory) Names(limit int) ([]string, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	directory.namesCalls++
	if limit <= 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	names := make([]string, 0, len(directory.dirs)+len(directory.files))
	for name := range directory.dirs {
		names = append(names, name)
	}
	for name := range directory.files {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func (directory *memoryDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}

func (directory *memoryDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.dirs[name]; found {
		return outputcap.EntryDirectory, true, nil
	}
	if _, found := directory.files[name]; found {
		return outputcap.EntryRegularFile, true, nil
	}
	return outputcap.EntryAbsent, true, nil
}

func (directory *memoryDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	peer, ok := other.(*memoryDirectory)
	return ok && directory == peer, nil
}

func (directory *memoryDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	child, found := directory.dirs[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return child, nil
}

func (directory *memoryDirectory) CreateDirectory(name string, _ bool) (outputcap.Directory, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	child := newMemoryDirectory()
	directory.dirs[name] = child
	return child, nil
}

func (directory *memoryDirectory) CreateFile(name string, _ bool, size int64) (outputcap.File, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	data := &memoryFileData{bytes: make([]byte, int(size))}
	directory.files[name] = data
	return &memoryFile{data: data}, nil
}

func (directory *memoryDirectory) OpenFile(name string, _, _ bool) (outputcap.File, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &memoryFile{data: data}, nil
}

func (directory *memoryDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	file, ok := source.(*memoryFile)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	directory.files[name] = file.data
	return &memoryFile{data: file.data}, nil
}

func (directory *memoryDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	file, ok := source.(*memoryFile)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	for candidate, data := range directory.files {
		if candidate != name && data == file.data {
			delete(directory.files, candidate)
			break
		}
	}
	directory.files[name] = file.data
	return nil
}

func (directory *memoryDirectory) RemoveFile(name string, expected outputcap.File) error {
	file, ok := expected.(*memoryFile)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return fs.ErrNotExist
	}
	if data != file.data {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.files, name)
	return nil
}

func (directory *memoryDirectory) AcquireLock(name string, _ bool) (outputcap.Lock, bool, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if lock, found := directory.locks[name]; found && !lock.closed {
		return nil, false, outputcap.ErrNamespaceLockBusy
	}
	data, found := directory.files[name]
	if !found {
		data = &memoryFileData{}
		directory.files[name] = data
	}
	lock := &memoryLock{directory: directory, name: name, file: &memoryFile{data: data}}
	directory.locks[name] = lock
	return lock, !found, nil
}

type memoryFileData struct {
	mu    sync.Mutex
	bytes []byte
}

type memoryFile struct {
	outputcap.File
	data *memoryFileData
}

func (file *memoryFile) SameFile(other outputcap.File) (bool, error) {
	peer, ok := other.(*memoryFile)
	return ok && file.data == peer.data, nil
}

func (file *memoryFile) Close() error { return nil }

func (file *memoryFile) Sync() error { return nil }

func (file *memoryFile) Size() (uint64, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	return uint64(len(file.data.bytes)), nil
}

func (file *memoryFile) ReadAt(target []byte, offset int64) (int, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	if offset < 0 || offset >= int64(len(file.data.bytes)) {
		return 0, io.EOF
	}
	read := copy(target, file.data.bytes[offset:])
	if read != len(target) {
		return read, io.EOF
	}
	return read, nil
}

func (file *memoryFile) WriteAt(source []byte, offset int64) (int, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	if offset < 0 || offset+int64(len(source)) > int64(len(file.data.bytes)) {
		return 0, io.ErrShortWrite
	}
	return copy(file.data.bytes[offset:], source), nil
}

type memoryLock struct {
	outputcap.Lock
	directory *memoryDirectory
	name      string
	file      *memoryFile
	closed    bool
}

func (lock *memoryLock) File() outputcap.File { return lock.file }

func (lock *memoryLock) Close() error {
	lock.directory.mu.Lock()
	defer lock.directory.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	delete(lock.directory.locks, lock.name)
	return nil
}

type shortWriteFile struct{ outputcap.File }

func (shortWriteFile) WriteAt(source []byte, _ int64) (int, error) { return len(source) - 1, nil }

func (shortWriteFile) Sync() error { return nil }
