package resumeauthority

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func certifiedFixture(
	t *testing.T,
	root outputcap.Directory,
	disposition checkpointmodel.RootOpenDisposition,
	fill byte,
) (checkpointstore.CertifiedConfig, transfer.TransferIntentDigest) {
	t.Helper()
	backend, err := transfer.NewOutputBackendID("resumeauthority-test")
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend: backend, Certification: checkpointmodel.CertificationWindowsNTFSProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{fill}, sha256.Size),
		RootOpenDisposition: disposition,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{fill + 1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return checkpointstore.CertifiedConfig{Root: root, Ownership: ownership}, intent
}

func checkpointRecordFixture(
	t *testing.T,
	ownership checkpointmodel.Ownership,
	intent transfer.TransferIntentDigest,
	fill byte,
) checkpointmodel.Record {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = fill
		revision[index] = fill + 1
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        "folder/file.bin",
		ExactSize:            64,
		BackendID:            string(ownership.Backend()),
		RootIdentity:         ownership.RootIdentity().Bytes(),
		OwnedOutputObject:    bytes.Repeat([]byte{fill + 2}, sha256.Size),
		StateGeneration:      1,
		CheckpointGeneration: 0,
		Phase:                checkpointmodel.PhaseActive,
		CommitState:          checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func onlyMemoryDirectory(t *testing.T, parent *memoryDirectory) (string, *memoryDirectory) {
	t.Helper()
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if len(parent.dirs) != 1 {
		t.Fatalf("directory children = %d, want 1", len(parent.dirs))
	}
	for name, child := range parent.dirs {
		return name, child
	}
	panic("unreachable")
}

func recoveryArtifactLocation(
	t *testing.T,
	object checkpointmodel.ObjectID,
	kind checkpointstore.RecoveryArtifactKind,
) (string, string) {
	t.Helper()
	shard, name, err := checkpointstore.RecoveryArtifactLocation(object, kind)
	if err != nil {
		t.Fatal(err)
	}
	return shard, name
}

func intentNamespaceNameForTest(intent transfer.TransferIntentDigest) string {
	return hex.EncodeToString(intent.Bytes())
}

func openOrCreateDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	kind, _, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryDirectory {
		return parent.OpenDirectory(name, true)
	}
	if kind != outputcap.EntryAbsent {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return parent.CreateDirectory(name, true)
}

func writeMemoryFile(t *testing.T, directory outputcap.Directory, name string, encoded []byte) {
	t.Helper()
	file, err := directory.CreateFile(name, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpointstore.WriteFile(file, encoded); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustRecordID(t *testing.T, fill byte) checkpointmodel.RecordID {
	t.Helper()
	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{fill}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return recordID
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
