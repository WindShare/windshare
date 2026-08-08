package resumeauthority

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

var (
	errResumeClosureClassify = errors.New("resume closure classification failed")
	errResumeClosureMatch    = errors.New("resume closure identity match failed")
	errResumeClosureNames    = errors.New("resume closure enumeration failed")
	errResumeClosureOpen     = errors.New("resume closure open failed")
	errResumeClosureSize     = errors.New("resume closure size failed")
	errResumeClosureVisit    = errors.New("resume closure visitor failed")
)

func resumeClosureCheckpointRoot(t *testing.T, root *memoryDirectory) *memoryDirectory {
	t.Helper()
	return root.dirsForTest(t, checkpointstore.ControlDirectory).dirsForTest(t, checkpointstore.CheckpointDirectory)
}

func resumeClosureIntentLayout(
	t *testing.T,
	fixture resumeAdapterFixture,
) (
	intent *memoryDirectory,
	intents *memoryDirectory,
	records *memoryDirectory,
	stages *memoryDirectory,
	anchors *memoryDirectory,
) {
	t.Helper()
	checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
	intents = checkpointRoot.dirsForTest(t, checkpointstore.IntentsDirectory)
	_, intent = onlyMemoryDirectory(t, intents)
	return intent, intents,
		intent.dirsForTest(t, checkpointstore.RecordsDirectory),
		intent.dirsForTest(t, checkpointstore.StagesDirectory),
		intent.dirsForTest(t, checkpointstore.AnchorsDirectory)
}

func resumeClosureRecordWithObject(
	t *testing.T,
	base checkpointmodel.Record,
	path string,
	fill byte,
	object checkpointmodel.ObjectID,
) checkpointmodel.Record {
	t.Helper()
	fileID := base.FileID()
	revision := base.FileRevision()
	fileID[0] = fill
	revision[0] = fill + 1
	candidate, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: base.TransferIntentDigest(),
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        path,
		ExactSize:            base.ExactSize(),
		BackendID:            string(base.BackendID()),
		RootIdentity:         base.RootIdentity().Bytes(),
		OwnedOutputObject:    object.Bytes(),
		StateGeneration:      1,
		CheckpointGeneration: 0,
		Phase:                checkpointmodel.PhaseActive,
		CommitState:          checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func resumeClosureScanOwner(
	t *testing.T,
) (*resumeOwnedDirectory, *memoryDirectory, *resumeClosureTree) {
	t.Helper()
	base := newMemoryDirectory()
	shardPort, err := base.CreateDirectory("aa", true)
	if err != nil {
		t.Fatal(err)
	}
	shard := shardPort.(*memoryDirectory)
	tree := newResumeClosureTree()
	return &resumeOwnedDirectory{
		name: checkpointstore.RecordsDirectory, directory: tree.wrap(base),
		shards: make(map[string]*resumeShardPins),
	}, shard, tree
}

type resumeClosureReference struct {
	outputcap.CurrentEntryReference
	kind outputcap.EntryKind
}

func (reference *resumeClosureReference) Kind() outputcap.EntryKind { return reference.kind }
func (*resumeClosureReference) Close() error                        { return nil }

type resumeClosureDirectoryBehavior struct {
	duplicate   func() (outputcap.Directory, error)
	names       func(int) ([]string, error)
	classify    func(string) (outputcap.EntryKind, bool, error)
	openEntry   func(string) (outputcap.CurrentEntryReference, error)
	entryMatch  func(string, outputcap.CurrentEntryReference) (bool, error)
	openPinned  func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error)
	openFile    func(string, bool, bool) (outputcap.File, error)
	removeEntry func(string, outputcap.CurrentEntryReference) error
	sync        func() error
	acquireLock func(string, bool) (outputcap.Lock, bool, error)
	closeErr    error
}

type resumeClosureTree struct {
	behaviors map[*memoryDirectory]*resumeClosureDirectoryBehavior
}

func newResumeClosureTree() *resumeClosureTree {
	return &resumeClosureTree{behaviors: make(map[*memoryDirectory]*resumeClosureDirectoryBehavior)}
}

func (tree *resumeClosureTree) behavior(
	directory *memoryDirectory,
) *resumeClosureDirectoryBehavior {
	behavior := tree.behaviors[directory]
	if behavior == nil {
		behavior = &resumeClosureDirectoryBehavior{}
		tree.behaviors[directory] = behavior
	}
	return behavior
}

func (tree *resumeClosureTree) wrap(directory *memoryDirectory) outputcap.Directory {
	return &resumeClosureTreeDirectory{Directory: directory, base: directory, tree: tree}
}

func (tree *resumeClosureTree) wrapResult(
	directory outputcap.Directory,
	err error,
) (outputcap.Directory, error) {
	if err != nil || directory == nil {
		return directory, err
	}
	if memory, ok := directory.(*memoryDirectory); ok {
		return tree.wrap(memory), nil
	}
	return directory, nil
}

type resumeClosureTreeDirectory struct {
	outputcap.Directory
	base *memoryDirectory
	tree *resumeClosureTree
}

func (directory *resumeClosureTreeDirectory) behavior() *resumeClosureDirectoryBehavior {
	return directory.tree.behavior(directory.base)
}

func (directory *resumeClosureTreeDirectory) Close() error {
	return errors.Join(directory.base.Close(), directory.behavior().closeErr)
}

func (directory *resumeClosureTreeDirectory) Duplicate() (outputcap.Directory, error) {
	if duplicate := directory.behavior().duplicate; duplicate != nil {
		return directory.tree.wrapResult(duplicate())
	}
	return directory.tree.wrapResult(directory.base.Duplicate())
}

func (directory *resumeClosureTreeDirectory) Names(limit int) ([]string, error) {
	if names := directory.behavior().names; names != nil {
		return names(limit)
	}
	return directory.base.Names(limit)
}

func (directory *resumeClosureTreeDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if classify := directory.behavior().classify; classify != nil {
		return classify(name)
	}
	return directory.base.ClassifyExactEntry(name)
}

func (directory *resumeClosureTreeDirectory) OpenEntry(
	name string,
) (outputcap.CurrentEntryReference, error) {
	if open := directory.behavior().openEntry; open != nil {
		return open(name)
	}
	return directory.base.OpenEntry(name)
}

func (directory *resumeClosureTreeDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	if match := directory.behavior().entryMatch; match != nil {
		return match(name, expected)
	}
	return directory.base.EntryMatches(name, expected)
}

func (directory *resumeClosureTreeDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	if open := directory.behavior().openPinned; open != nil {
		return directory.tree.wrapResult(open(expected, private))
	}
	return directory.tree.wrapResult(directory.base.OpenPinnedDirectory(expected, private))
}

func (directory *resumeClosureTreeDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if open := directory.behavior().openFile; open != nil {
		return open(name, private, writable)
	}
	return directory.base.OpenFile(name, private, writable)
}

func (directory *resumeClosureTreeDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	if remove := directory.behavior().removeEntry; remove != nil {
		return remove(name, expected)
	}
	return directory.base.RemoveEntry(name, expected)
}

func (directory *resumeClosureTreeDirectory) Sync() error {
	if syncDirectory := directory.behavior().sync; syncDirectory != nil {
		return syncDirectory()
	}
	return directory.base.Sync()
}

func (directory *resumeClosureTreeDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if acquire := directory.behavior().acquireLock; acquire != nil {
		return acquire(name, existingOnly)
	}
	return directory.base.AcquireLock(name, existingOnly)
}

type resumeClosureFile struct {
	outputcap.File
	closeErr error
	sizeErr  error
}

func (file *resumeClosureFile) Close() error {
	return errors.Join(file.File.Close(), file.closeErr)
}

func (file *resumeClosureFile) Size() (uint64, error) {
	if file.sizeErr != nil {
		return 0, file.sizeErr
	}
	return file.File.Size()
}

type resumeClosureLock struct {
	outputcap.Lock
	closeErr error
}

func (lock *resumeClosureLock) Close() error {
	return errors.Join(lock.Lock.Close(), lock.closeErr)
}

type resumeClosureTrackedDirectory struct {
	outputcap.Directory
	name     string
	log      *[]string
	closeErr error
}

func (directory *resumeClosureTrackedDirectory) Close() error {
	*directory.log = append(*directory.log, directory.name)
	return directory.closeErr
}

type resumeClosureTrackedReference struct {
	outputcap.CurrentEntryReference
	name     string
	log      *[]string
	closeErr error
}

func (reference *resumeClosureTrackedReference) Close() error {
	*reference.log = append(*reference.log, reference.name)
	return reference.closeErr
}

type resumeClosureTrackedLock struct {
	outputcap.Lock
	name     string
	log      *[]string
	closeErr error
}

type resumeClosureLease struct {
	lock outputcap.Lock
}

func (lease resumeClosureLease) Binding() checkpointmodel.Binding {
	return checkpointmodel.Binding{}
}

func (lease resumeClosureLease) Close() error {
	return lease.lock.Close()
}

func (lock *resumeClosureTrackedLock) Close() error {
	*lock.log = append(*lock.log, lock.name)
	return lock.closeErr
}
