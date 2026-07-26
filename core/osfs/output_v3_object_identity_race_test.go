package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const v3RecoveryObjectClaimSerializationWindow = 2 * time.Second

func TestOutputV3ConcurrentFilesCannotPersistTheSameOutputObject(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"first.bin", "second.bin"}, 1)
	sessionIDs := &v3RecoverySessionIDs{}
	duplicateID := v3RecoveryOutputObjectID(t, 0xd1)
	objectIDs := &v3RecoveryDuplicateObjectIDs{duplicate: duplicateID}
	observationGate := &v3RecoveryObjectObservationGate{
		target:  resumestate.StageName(duplicateID).Name(),
		first:   make(chan struct{}),
		second:  make(chan struct{}),
		release: make(chan struct{}),
	}
	defer observationGate.releaseAll()

	authority := v3RecoveryAuthority(t, root, sessionIDs)
	authority.objectIDs = objectIDs
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapObjectPlatform(platform, observationGate), nil
	}
	opened := v3RecoveryOpen(t, authority, root, selection)
	session := opened.Session
	v3RecoveryCreateObjectShard(t, session.stagesDir, resumestate.StageName(duplicateID).Shard())
	v3RecoveryCreateObjectShard(t, session.anchorsDir, resumestate.AnchorName(duplicateID).Shard())

	type beginResult struct {
		start transfer.FileStart
		err   error
	}
	results := make(chan beginResult, 2)
	files := []transfer.OutputFile{
		v3RecoveryOutputFileAt(t, session, selection, 0),
		v3RecoveryOutputFileAt(t, session, selection, 1),
	}
	go func() {
		start, err := session.BeginFile(context.Background(), files[0])
		results <- beginResult{start: start, err: err}
	}()
	select {
	case <-observationGate.first:
	case <-time.After(v3RecoveryLockGateTimeout):
		t.Fatal("first file did not reach output-object occupancy observation")
	}
	go func() {
		start, err := session.BeginFile(context.Background(), files[1])
		results <- beginResult{start: start, err: err}
	}()
	select {
	case <-observationGate.second:
		// The unfixed implementation lets both callers cache an absent witness.
		// Releasing them together deterministically exercises that interleaving.
	case <-time.After(v3RecoveryObjectClaimSerializationWindow):
		// A correct session-wide claim keeps the second caller out until the first
		// has installed its record and witnesses. Release the serialized owner.
	}
	observationGate.releaseAll()

	var beginErrors []error
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				beginErrors = append(beginErrors, result.err)
			}
			v3RecoveryAbandonStartedFile(t, session, result.start)
		case <-time.After(v3RecoveryLockGateTimeout):
			t.Fatal("concurrent file start did not settle")
		}
	}
	sessionID := session.SessionID()
	v3RecoveryCloseSession(t, session)

	persisted := v3RecoveryReadPersistedOutputObjects(t, root, selection, sessionID)
	persistedDuplicate := v3RecoveryDuplicateOutputObject(persisted)

	reopenAuthority := v3RecoveryAuthority(t, root, sessionIDs)
	reopenAuthority.objectIDs = objectIDs
	reopened, err := v3OpenSelection(context.Background(), reopenAuthority, selection)
	if err != nil {
		if persistedDuplicate != "" {
			t.Fatalf("concurrent starts persisted duplicate output-object ownership for %s before restart rejection: %v",
				persistedDuplicate, err)
		}
		t.Fatal(err)
	}
	resumed := make(map[resumestate.OutputObjectID]string)
	crossAdoption := ""
	for index, file := range selection.Files() {
		start, err := reopened.Session.BeginFile(
			context.Background(), v3RecoveryOutputFileAt(t, reopened.Session, selection, index),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			continue
		}
		concrete := transaction.(*filesystemFileTransaction)
		objectID := concrete.resumable.Bound().Record().OutputObject()
		if previous, exists := resumed[objectID]; exists {
			crossAdoption = previous + " and " + file.Path
		}
		resumed[objectID] = file.Path
		v3RecoveryAbandonStartedFile(t, reopened.Session, start)
	}
	v3RecoveryCloseSession(t, reopened.Session)
	if persistedDuplicate != "" {
		t.Fatalf("concurrent starts persisted duplicate output-object ownership for %s", persistedDuplicate)
	}
	if crossAdoption != "" {
		t.Fatalf("restart cross-adopted an output object for %s", crossAdoption)
	}
	if len(beginErrors) != 0 {
		t.Fatalf("duplicate object-ID allocation did not retry safely: %v", errors.Join(beginErrors...))
	}
}

func v3RecoveryOutputObjectID(t *testing.T, value byte) resumestate.OutputObjectID {
	t.Helper()
	id, err := resumestate.OutputObjectIDFromBytes(
		v3RecoveryIdentityBytes(value, resumestate.OutputObjectIDBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func v3RecoveryIdentityBytes(value byte, size int) []byte {
	identity := make([]byte, size)
	for index := range identity {
		identity[index] = value
	}
	return identity
}

type v3RecoveryDuplicateObjectIDs struct {
	mu        sync.Mutex
	calls     int
	duplicate resumestate.OutputObjectID
}

func (generator *v3RecoveryDuplicateObjectIDs) NewOutputObjectID() (resumestate.OutputObjectID, error) {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.calls++
	if generator.calls <= 2 {
		return generator.duplicate, nil
	}
	value := byte(0xd1 + generator.calls)
	return resumestate.OutputObjectIDFromBytes(
		v3RecoveryIdentityBytes(value, resumestate.OutputObjectIDBytes),
	)
}

func v3RecoveryCreateObjectShard(t *testing.T, parent outputV3Directory, name string) {
	t.Helper()
	shard, err := parent.CreateDirectory(name, true)
	if errors.Is(err, errOutputV3Collision) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(shard.Sync(), parent.Sync(), shard.Close()); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryAbandonStartedFile(
	t *testing.T,
	session *filesystemOutputSession,
	start transfer.FileStart,
) {
	t.Helper()
	transaction, _, ok := start.Transaction()
	if !ok {
		return
	}
	concrete := transaction.(*filesystemFileTransaction)
	digest := concrete.resumable.Bound().Record().LocatorDigest()
	concrete.lifecycle = filesystemFileTransactionClosed
	if err := concrete.closeHandles(); err != nil {
		t.Fatal(err)
	}
	session.finishFile(digest, concrete)
}

func v3RecoveryReadPersistedOutputObjects(
	t *testing.T,
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
) map[string]resumestate.OutputObjectID {
	t.Helper()
	objects := make(map[string]resumestate.OutputObjectID)
	base := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID), resumestate.FilesDirectoryName,
	)
	for _, file := range selection.Files() {
		name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(file.Path))
		encoded, err := os.ReadFile(filepath.Join(base, name.Shard(), name.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		record, err := resumestate.DecodeFileRecord(encoded)
		if err != nil {
			t.Fatal(err)
		}
		objects[file.Path] = record.OutputObject()
	}
	return objects
}

func v3RecoveryDuplicateOutputObject(objects map[string]resumestate.OutputObjectID) string {
	owners := make(map[resumestate.OutputObjectID]string)
	for path, objectID := range objects {
		if previous, exists := owners[objectID]; exists {
			return previous + " and " + path
		}
		owners[objectID] = path
	}
	return ""
}

type v3RecoveryObjectObservationGate struct {
	mu       sync.Mutex
	target   string
	arrivals int
	first    chan struct{}
	second   chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (gate *v3RecoveryObjectObservationGate) observeAbsent() {
	gate.mu.Lock()
	gate.arrivals++
	switch gate.arrivals {
	case 1:
		close(gate.first)
	case 2:
		close(gate.second)
	}
	release := gate.release
	gate.mu.Unlock()
	<-release
}

func (gate *v3RecoveryObjectObservationGate) releaseAll() {
	gate.once.Do(func() { close(gate.release) })
}

type v3RecoveryObjectPlatform struct {
	outputV3Platform
	root outputV3Directory
}

func v3RecoveryWrapObjectPlatform(
	platform outputV3Platform,
	gate *v3RecoveryObjectObservationGate,
) outputV3Platform {
	return &v3RecoveryObjectPlatform{
		outputV3Platform: platform,
		root:             v3RecoveryWrapObjectDirectory(platform.Root(), gate),
	}
}

func (platform *v3RecoveryObjectPlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryObjectPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryObjectDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapObjectDirectory(root, decorated.gate)
		},
	)
}

type v3RecoveryObjectDirectory struct {
	outputV3Directory
	gate *v3RecoveryObjectObservationGate
}

func v3RecoveryWrapObjectDirectory(
	directory outputV3Directory,
	gate *v3RecoveryObjectObservationGate,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryObjectDirectory{outputV3Directory: directory, gate: gate}
}

func v3RecoveryUnwrapObjectDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryObjectDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func (directory *v3RecoveryObjectDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapObjectDirectory(duplicate, directory.gate), err
}

func (directory *v3RecoveryObjectDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapObjectDirectory(other))
}

func (directory *v3RecoveryObjectDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	kind, err := directory.outputV3Directory.ObserveEntry(name)
	if err == nil && kind == outputV3EntryAbsent && name == directory.gate.target {
		directory.gate.observeAbsent()
	}
	return kind, err
}

func (directory *v3RecoveryObjectDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapObjectDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryObjectDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapObjectDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryObjectDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapObjectDirectory(created, directory.gate), err
}

func (directory *v3RecoveryObjectDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapObjectDirectory(candidate), name,
	)
	return v3RecoveryWrapObjectDirectory(installed, directory.gate), err
}

func (directory *v3RecoveryObjectDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapObjectDirectory(expected),
	)
}
