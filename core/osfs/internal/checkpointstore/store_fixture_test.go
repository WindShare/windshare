package checkpointstore

import (
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type faultDirectory struct {
	outputcap.Directory
	duplicate       func() (outputcap.Directory, error)
	names           func(int) ([]string, error)
	openDirectory   func(string, bool) (outputcap.Directory, error)
	createDirectory func(string, bool) (outputcap.Directory, error)
	createFile      func(string, bool, int64) (outputcap.MutableFile, error)
	openObserved    func(string, bool) (outputcap.ObservedFile, error)
	openFile        func(string, bool, bool) (outputcap.MutableFile, error)
	openRecovery    func(string, bool) (outputcap.RecoveryDurabilityFile, error)
	linkFile        func(outputcap.FileIdentity, string) (outputcap.ObservedFile, error)
	replaceFile     func(outputcap.FileIdentity, string) error
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

func (directory *faultDirectory) CreateFile(name string, private bool, size int64) (outputcap.MutableFile, error) {
	if directory.createFile != nil {
		return directory.createFile(name, private, size)
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *faultDirectory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	if directory.openObserved != nil {
		return directory.openObserved(name, private)
	}
	if directory.openFile != nil {
		return directory.openFile(name, private, false)
	}
	return directory.Directory.OpenObservedFile(name, private)
}

func (directory *faultDirectory) OpenRecoveryDurabilityFile(name string, private bool) (outputcap.RecoveryDurabilityFile, error) {
	if directory.openRecovery != nil {
		return directory.openRecovery(name, private)
	}
	if directory.openFile != nil {
		return directory.openFile(name, private, false)
	}
	return directory.Directory.OpenRecoveryDurabilityFile(name, private)
}

func (directory *faultDirectory) OpenMutableFile(name string, private bool) (outputcap.MutableFile, error) {
	if directory.openFile != nil {
		return directory.openFile(name, private, true)
	}
	return directory.Directory.OpenMutableFile(name, private)
}

func (directory *faultDirectory) LinkFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	if directory.linkFile != nil {
		return directory.linkFile(source, name)
	}
	return directory.Directory.LinkFileNoReplace(source, name)
}

func (directory *faultDirectory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
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
	outputcap.MutableFile
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)
}

func (file *faultFile) ReadAt(encoded []byte, offset int64) (int, error) {
	if file.readAt != nil {
		return file.readAt(encoded, offset)
	}
	return file.MutableFile.ReadAt(encoded, offset)
}

func (file *faultFile) WriteAt(encoded []byte, offset int64) (int, error) {
	return file.writeAt(encoded, offset)
}

func (file *faultFile) Close() error { return nil }

type closeTrackingFile struct {
	outputcap.MutableFile
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

func (directory *memoryDirectory) CreateFile(name string, _ bool, size int64) (outputcap.MutableFile, error) {
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

func (directory *memoryDirectory) OpenObservedFile(name string, _ bool) (outputcap.ObservedFile, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &memoryObservedFile{data: data}, nil
}

func (directory *memoryDirectory) OpenRecoveryDurabilityFile(name string, _ bool) (outputcap.RecoveryDurabilityFile, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &memoryRecoveryFile{data: data}, nil
}

func (directory *memoryDirectory) OpenMutableFile(name string, _ bool) (outputcap.MutableFile, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &memoryFile{data: data}, nil
}

func (directory *memoryDirectory) LinkFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	data, ok := memoryFileDataFrom(source)
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
	directory.files[name] = data
	return &memoryObservedFile{data: data}, nil
}

func (directory *memoryDirectory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	data, ok := memoryFileDataFrom(source)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	for candidate, candidateData := range directory.files {
		if candidate != name && candidateData == data {
			delete(directory.files, candidate)
			break
		}
	}
	directory.files[name] = data
	return nil
}

func (directory *memoryDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	expectedData, ok := memoryFileDataFrom(expected)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return fs.ErrNotExist
	}
	if data != expectedData {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.files, name)
	return nil
}

func (directory *memoryDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	child, ok := expected.(*memoryDirectory)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	actual, found := directory.dirs[name]
	if !found {
		return fs.ErrNotExist
	}
	if actual != child {
		return outputcap.ErrUnsafeNamespace
	}
	child.mu.Lock()
	empty := len(child.dirs) == 0 && len(child.files) == 0
	child.mu.Unlock()
	if !empty {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.dirs, name)
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
	data *memoryFileData
}

type memoryObservedFile struct {
	data *memoryFileData
}

type memoryRecoveryFile struct {
	data *memoryFileData
}

func memoryFileDataFrom(file outputcap.FileIdentity) (*memoryFileData, bool) {
	switch value := file.(type) {
	case *memoryFile:
		return value.data, value != nil && value.data != nil
	case *memoryObservedFile:
		return value.data, value != nil && value.data != nil
	case *memoryRecoveryFile:
		return value.data, value != nil && value.data != nil
	default:
		return nil, false
	}
}

func (file *memoryObservedFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	peer, ok := memoryFileDataFrom(other)
	return ok && file.data == peer, nil
}
func (file *memoryObservedFile) Close() error { return nil }
func (file *memoryObservedFile) Size() (uint64, error) {
	return (&memoryFile{data: file.data}).Size()
}
func (file *memoryObservedFile) ReadAt(target []byte, offset int64) (int, error) {
	return (&memoryFile{data: file.data}).ReadAt(target, offset)
}
func (file *memoryObservedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	return (&memoryFile{data: file.data}).MetadataMatches(size, modified)
}

func (file *memoryRecoveryFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	peer, ok := memoryFileDataFrom(other)
	return ok && file.data == peer, nil
}
func (file *memoryRecoveryFile) Close() error { return nil }
func (file *memoryRecoveryFile) Size() (uint64, error) {
	return (&memoryFile{data: file.data}).Size()
}
func (file *memoryRecoveryFile) Sync() error { return nil }

func (file *memoryFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	peer, ok := memoryFileDataFrom(other)
	return ok && file.data == peer, nil
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

func (lock *memoryLock) File() outputcap.MutableFile { return lock.file }

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

type shortWriteFile struct{ outputcap.MutableFile }

func (shortWriteFile) WriteAt(source []byte, _ int64) (int, error) { return len(source) - 1, nil }

func (shortWriteFile) Sync() error { return nil }
