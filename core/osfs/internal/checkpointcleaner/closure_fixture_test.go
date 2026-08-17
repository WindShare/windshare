package checkpointcleaner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type c5ClosureEntry struct {
	kind  outputcap.EntryKind
	exact bool
}

type c5ClosureDirectory struct {
	outputcap.Directory
	entries  map[string]c5ClosureEntry
	children map[string]*c5ClosureDirectory
	names    func(int) ([]string, error)
	close    func() error
}

func (directory *c5ClosureDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	names := make([]string, 0, len(directory.entries))
	for name := range directory.entries {
		names = append(names, name)
	}
	return names, nil
}

func (directory *c5ClosureDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	entry, ok := directory.entries[name]
	if !ok {
		return outputcap.EntryAbsent, true, nil
	}
	return entry.kind, entry.exact, nil
}

func (directory *c5ClosureDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	child, ok := directory.children[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return child, nil
}

func (directory *c5ClosureDirectory) Close() error {
	if directory.close == nil {
		return nil
	}
	return directory.close()
}

type c5ClosurePlatform struct {
	outputcap.Platform
	root          outputcap.Directory
	guard         outputcap.PublicOperationGuard
	acquireErr    error
	binding       outputcap.OutputRootBinding
	bindingErr    error
	certification outputcap.CertificationID
	durability    transfer.DurabilityLevel
}

func (platform *c5ClosurePlatform) Root() outputcap.Directory {
	return platform.root
}

func (platform *c5ClosurePlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	return platform.guard, platform.acquireErr
}

func (platform *c5ClosurePlatform) RootBinding() (outputcap.OutputRootBinding, error) {
	return platform.binding, platform.bindingErr
}

func (platform *c5ClosurePlatform) Certification() outputcap.CertificationID {
	return platform.certification
}

func (platform *c5ClosurePlatform) Durability() transfer.DurabilityLevel {
	return platform.durability
}

type c5ClosureStaticGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *c5ClosureStaticGuard) Root() outputcap.Directory { return guard.root }

func (guard *c5ClosureStaticGuard) Close() error { return nil }

type c5ClosureFaultDirectory struct {
	outputcap.Directory
	classify        func(string) (outputcap.EntryKind, bool, error)
	names           func(int) ([]string, error)
	openDirectory   func(string, bool) (outputcap.Directory, error)
	createDirectory func(string, bool) (outputcap.Directory, error)
	openFile        func(string, bool, bool) (outputcap.MutableFile, error)
	openEntry       func(string) (outputcap.CurrentEntryReference, error)
	openPinned      func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error)
	entryMatches    func(string, outputcap.CurrentEntryReference) (bool, error)
	removeEntry     func(string, outputcap.CurrentEntryReference) error
	removeDirectory func(string, outputcap.Directory) error
	removeFile      func(string, outputcap.FileIdentity) error
	acquireLock     func(string, bool) (outputcap.Lock, bool, error)
	duplicate       func() (outputcap.Directory, error)
	sameDirectory   func(outputcap.Directory) (bool, error)
	syncErr         error
	closeErr        error
}

func (directory *c5ClosureFaultDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory.classify == nil {
		return outputcap.EntryAbsent, true, nil
	}
	return directory.classify(name)
}

func (directory *c5ClosureFaultDirectory) Names(limit int) ([]string, error) {
	if directory.names == nil {
		return nil, nil
	}
	return directory.names(limit)
}

func (directory *c5ClosureFaultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.openDirectory == nil {
		return nil, fs.ErrNotExist
	}
	return directory.openDirectory(name, private)
}

func (directory *c5ClosureFaultDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.createDirectory == nil {
		return nil, errors.New("unexpected directory creation")
	}
	return directory.createDirectory(name, private)
}

func (directory *c5ClosureFaultDirectory) OpenObservedFile(name string, private bool) (outputcap.ObservedFile, error) {
	if directory.openFile == nil {
		return nil, fs.ErrNotExist
	}
	return directory.openFile(name, private, false)
}

func (directory *c5ClosureFaultDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory.openEntry == nil {
		return nil, fs.ErrNotExist
	}
	return directory.openEntry(name)
}

func (directory *c5ClosureFaultDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	if directory.openPinned == nil {
		return nil, errors.New("unexpected pinned directory open")
	}
	return directory.openPinned(expected, private)
}

func (directory *c5ClosureFaultDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	if directory.entryMatches == nil {
		return false, errors.New("unexpected entry comparison")
	}
	return directory.entryMatches(name, expected)
}

func (directory *c5ClosureFaultDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	if directory.removeEntry == nil {
		return errors.New("unexpected entry removal")
	}
	return directory.removeEntry(name, expected)
}

func (directory *c5ClosureFaultDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	if directory.removeDirectory == nil {
		return errors.New("unexpected directory removal")
	}
	return directory.removeDirectory(name, expected)
}

func (directory *c5ClosureFaultDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	if directory.removeFile == nil {
		return errors.New("unexpected file removal")
	}
	return directory.removeFile(name, expected)
}

func (directory *c5ClosureFaultDirectory) AcquireLock(name string, existingOnly bool) (outputcap.Lock, bool, error) {
	if directory.acquireLock == nil {
		return nil, false, errors.New("unexpected lock acquisition")
	}
	return directory.acquireLock(name, existingOnly)
}

func (directory *c5ClosureFaultDirectory) Duplicate() (outputcap.Directory, error) {
	if directory.duplicate == nil {
		return nil, errors.New("unexpected directory duplication")
	}
	return directory.duplicate()
}

func (directory *c5ClosureFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if directory.sameDirectory == nil {
		return false, errors.New("unexpected directory comparison")
	}
	return directory.sameDirectory(other)
}

func (directory *c5ClosureFaultDirectory) Sync() error { return directory.syncErr }

func (directory *c5ClosureFaultDirectory) Close() error { return directory.closeErr }

type c5ClosureFaultFile struct {
	outputcap.MutableFile
	data     []byte
	size     uint64
	sizeErr  error
	read     func([]byte, int64) (int, error)
	same     func(outputcap.FileIdentity) (bool, error)
	closeErr error
}

func (file *c5ClosureFaultFile) Size() (uint64, error) {
	if file.size == 0 && len(file.data) != 0 {
		return uint64(len(file.data)), file.sizeErr
	}
	return file.size, file.sizeErr
}

func (file *c5ClosureFaultFile) ReadAt(buffer []byte, offset int64) (int, error) {
	if file.read != nil {
		return file.read(buffer, offset)
	}
	if offset < 0 || offset >= int64(len(file.data)) {
		return 0, io.EOF
	}
	read := copy(buffer, file.data[offset:])
	if read != len(buffer) {
		return read, io.EOF
	}
	return read, nil
}

func (file *c5ClosureFaultFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	if file.same == nil {
		return file == other, nil
	}
	return file.same(other)
}

func (file *c5ClosureFaultFile) Close() error { return file.closeErr }

type c5ClosureFaultLock struct {
	outputcap.Lock
	file  outputcap.MutableFile
	close func() error
}

func (lock *c5ClosureFaultLock) File() outputcap.MutableFile { return lock.file }

func (lock *c5ClosureFaultLock) Close() error {
	if lock.close == nil {
		return nil
	}
	return lock.close()
}

type c5ClosureCloseDirectory struct {
	outputcap.Directory
	name  string
	order *[]string
	err   error
}

func (directory *c5ClosureCloseDirectory) Close() error {
	*directory.order = append(*directory.order, directory.name)
	return directory.err
}

type c5ClosureCloseLock struct {
	outputcap.Lock
	name  string
	order *[]string
	err   error
}

func (lock *c5ClosureCloseLock) File() outputcap.MutableFile { return nil }

func (lock *c5ClosureCloseLock) Close() error {
	*lock.order = append(*lock.order, lock.name)
	return lock.err
}

type c5ClosureCloseGuard struct {
	outputcap.PublicOperationGuard
	name  string
	order *[]string
	err   error
}

func (guard *c5ClosureCloseGuard) Close() error {
	*guard.order = append(*guard.order, guard.name)
	return guard.err
}

type c5ClosureTracker struct {
	locks                  []string
	coordinatorRemoved     bool
	coordinatorSyncFailure error
	coordinatorSyncFailed  bool
}

type c5ClosureTrackedPlatform struct {
	outputcap.Platform
	tracker *c5ClosureTracker
}

func (platform *c5ClosureTrackedPlatform) Root() outputcap.Directory {
	return platform.wrapDirectory("", platform.Platform.Root())
}

func (platform *c5ClosureTrackedPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil || guard == nil {
		return guard, err
	}
	return &c5ClosureTrackedGuard{PublicOperationGuard: guard, platform: platform}, nil
}

func (platform *c5ClosureTrackedPlatform) wrapDirectory(relative string, directory outputcap.Directory) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &c5ClosureTrackedDirectory{Directory: directory, platform: platform, relative: relative}
}

type c5ClosureTrackedGuard struct {
	outputcap.PublicOperationGuard
	platform *c5ClosureTrackedPlatform
}

func (guard *c5ClosureTrackedGuard) Root() outputcap.Directory {
	return guard.platform.wrapDirectory("", guard.PublicOperationGuard.Root())
}

type c5ClosureTrackedDirectory struct {
	outputcap.Directory
	platform *c5ClosureTrackedPlatform
	relative string
}

func (directory *c5ClosureTrackedDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	child, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(path.Join(directory.relative, name), child), nil
}

func (directory *c5ClosureTrackedDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	child, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(path.Join(directory.relative, name), child), nil
}

func (directory *c5ClosureTrackedDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if tracked, ok := other.(*c5ClosureTrackedDirectory); ok {
		other = tracked.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *c5ClosureTrackedDirectory) AcquireLock(name string, existingOnly bool) (outputcap.Lock, bool, error) {
	lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
	if err == nil {
		directory.platform.tracker.locks = append(
			directory.platform.tracker.locks, path.Join(directory.relative, name),
		)
	}
	return lock, created, err
}

func (directory *c5ClosureTrackedDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	err := directory.Directory.RemoveFile(name, expected)
	if err == nil && path.Join(directory.relative, name) == path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock) {
		directory.platform.tracker.coordinatorRemoved = true
	}
	return err
}

func (directory *c5ClosureTrackedDirectory) Sync() error {
	err := directory.Directory.Sync()
	tracker := directory.platform.tracker
	if directory.relative == legacyresume.ControlDirectory && tracker.coordinatorRemoved &&
		!tracker.coordinatorSyncFailed && tracker.coordinatorSyncFailure != nil {
		tracker.coordinatorSyncFailed = true
		return errors.Join(err, tracker.coordinatorSyncFailure)
	}
	return err
}

func c5ClosureRecordDirectory(file outputcap.MutableFile) *c5ClosureFaultDirectory {
	return &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
		openFile: func(string, bool, bool) (outputcap.MutableFile, error) { return file, nil },
	}
}

func c5ClosureRootBinding(t *testing.T, object string) outputcap.OutputRootBinding {
	t.Helper()
	binding, err := outputcap.NewOutputRootBinding(
		outputcap.CertificationWindowsNTFSProcessRestart,
		[]byte("test-volume"), []byte(object),
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func c5ClosureStateRun(t *testing.T) (*cleanupRun, []byte) {
	t.Helper()
	run := &cleanupRun{
		cleaner: &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
			BackendID: legacyresume.NativeFilesystemBackend,
		}},
		rootBinding:   bytes.Repeat([]byte{0x71}, legacyresume.RootIdentityBytes),
		certification: legacyresume.CertificationWindowsNTFSProcessRestart,
		durability:    transfer.DurabilityProcessRestart,
	}
	state := c5ClosureCanonicalState(run)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return run, encoded
}

func c5ClosureCanonicalState(run *cleanupRun) cleanerState {
	state := cleanerState{
		Schema: cleanerStateSchema, BackendID: string(run.cleaner.config.BackendID),
		Certification: run.certification, RootIdentity: append([]byte(nil), run.rootBinding...),
		Durability: uint8(run.durability), RunGeneration: 1, Complete: true,
	}
	state.Checksum = stateChecksum(state)
	return state
}

func c5ClosureStateDirectory(encoded []byte) *c5ClosureFaultDirectory {
	return &c5ClosureFaultDirectory{
		classify: func(name string) (outputcap.EntryKind, bool, error) {
			if name == FileCheckpointCleanupState {
				return outputcap.EntryRegularFile, true, nil
			}
			return outputcap.EntryAbsent, true, nil
		},
		openFile: func(string, bool, bool) (outputcap.MutableFile, error) {
			return &c5ClosureFaultFile{data: append([]byte(nil), encoded...)}, nil
		},
	}
}

type c5ClosureMutationFixture struct {
	run                *cleanupRun
	platform           *c5ClosurePlatform
	root               *c5ClosureFaultDirectory
	control            *c5ClosureFaultDirectory
	namespace          *c5ClosureFaultDirectory
	stateFile          *c5ClosureFaultFile
	cleanupCurrent     *c5ClosureFaultFile
	coordinatorCurrent *c5ClosureFaultFile
	state              []byte
}

func c5ClosureAuthorizedMutation(t *testing.T) c5ClosureMutationFixture {
	t.Helper()
	binding := c5ClosureRootBinding(t, "authorized-root")
	state := []byte("canonical cleaner state")
	stateFile := &c5ClosureFaultFile{data: append([]byte(nil), state...)}
	cleanupExpected := &c5ClosureFaultFile{}
	coordinatorExpected := &c5ClosureFaultFile{}
	cleanupCurrent := &c5ClosureFaultFile{same: func(other outputcap.FileIdentity) (bool, error) {
		return other == cleanupExpected, nil
	}}
	coordinatorCurrent := &c5ClosureFaultFile{same: func(other outputcap.FileIdentity) (bool, error) {
		return other == coordinatorExpected, nil
	}}
	namespace := &c5ClosureFaultDirectory{
		classify: func(name string) (outputcap.EntryKind, bool, error) {
			if name == FileCheckpointCleanupState {
				return outputcap.EntryRegularFile, true, nil
			}
			return outputcap.EntryAbsent, true, nil
		},
		openFile: func(name string, _, _ bool) (outputcap.MutableFile, error) {
			switch name {
			case FileCheckpointCleanupState:
				return stateFile, nil
			case FileCheckpointCleanupLock:
				return cleanupCurrent, nil
			default:
				return nil, fs.ErrNotExist
			}
		},
		sameDirectory: func(outputcap.Directory) (bool, error) { return true, nil },
	}
	control := &c5ClosureFaultDirectory{
		openDirectory: func(name string, _ bool) (outputcap.Directory, error) {
			if name == legacyresume.CheckpointDirectory {
				return namespace, nil
			}
			return nil, fs.ErrNotExist
		},
		openFile: func(name string, _, _ bool) (outputcap.MutableFile, error) {
			if name == legacyresume.CoordinatorLock {
				return coordinatorCurrent, nil
			}
			return nil, fs.ErrNotExist
		},
		sameDirectory: func(outputcap.Directory) (bool, error) { return true, nil },
	}
	root := &c5ClosureFaultDirectory{
		openDirectory: func(name string, _ bool) (outputcap.Directory, error) {
			if name == legacyresume.ControlDirectory {
				return control, nil
			}
			return nil, fs.ErrNotExist
		},
		sameDirectory: func(outputcap.Directory) (bool, error) { return true, nil },
	}
	platform := &c5ClosurePlatform{
		root: root, binding: binding,
		certification: outputcap.CertificationWindowsNTFSProcessRestart,
		durability:    transfer.DurabilityProcessRestart,
	}
	run := &cleanupRun{
		cleaner: &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
			Platform: platform, BackendID: legacyresume.NativeFilesystemBackend,
		}},
		root: root, rootBinding: binding.Bytes(), certification: string(platform.certification),
		durability: transfer.DurabilityProcessRestart, control: control, namespace: namespace,
		cleanupLock: &c5ClosureFaultLock{file: cleanupExpected},
		coordinator: &c5ClosureFaultLock{file: coordinatorExpected},
		approved:    make(map[string]outputcap.EntryKind),
	}
	return c5ClosureMutationFixture{
		run: run, platform: platform, root: root, control: control, namespace: namespace,
		stateFile: stateFile, cleanupCurrent: cleanupCurrent,
		coordinatorCurrent: coordinatorCurrent, state: state,
	}
}

func c5ClosureHasEntry(report CheckpointCleanupReport, relative, detail string) bool {
	for _, entry := range report.Entries {
		if entry.RelativePath == relative && entry.Detail == detail {
			return true
		}
	}
	return false
}

func c5ClosureStepIndex(steps []CheckpointCleanupStep, relative string) int {
	for index, step := range steps {
		if step.RelativePath == relative {
			return index
		}
	}
	return -1
}
