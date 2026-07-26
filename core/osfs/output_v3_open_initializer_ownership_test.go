package osfs

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3OpenInitializerSynchronouslyClosesEveryOwnerAfterUncertainAdoption(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"duplicate-a.bin", "duplicate-b.bin"}, 1)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	duplicateObject := v3RecoveryOutputObjectID(t, 0xe1)
	targetShard, targetRecord := v3RecoveryInstallDuplicateReservedRecords(
		t, opened.Session, selection, duplicateObject,
	)
	sessionID := opened.Session.SessionID()
	v3RecoveryCloseSession(t, opened.Session)

	tracker := newV3RecoveryInitializerCloseTracker(root, selection, sessionID)
	fault := v3RecoveryInitializerFault{
		shardPath: filepath.Join(
			v3RecoverySessionPath(root, selection, sessionID), resumestate.FilesDirectoryName, targetShard,
		),
		recordName: targetRecord,
	}
	reopenAuthority := v3RecoveryAuthority(t, root, sessionIDs)
	reopenAuthority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapInitializerPlatform(platform, path, tracker, fault), nil
	}
	result, err := v3OpenSelection(context.Background(), reopenAuthority, selection)
	if result.Session != nil || !errors.Is(err, errStateStoreInjected) ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultFile {
		t.Fatalf("uncertain open-time adoption = (session=%v, err=%v), want unexposed file fault", result.Session, err)
	}

	// No exposed session exists to own an asynchronous poison teardown. Returning
	// from OpenSelection is therefore the ownership barrier: every acquired handle
	// must already have one, and only one, matching close.
	tracker.assertExactlyOnce(t,
		v3RecoveryCloseFiles,
		v3RecoveryCloseAnchors,
		v3RecoveryCloseStages,
		v3RecoveryCloseSessionLock,
		v3RecoveryCloseSessionDirectory,
		v3RecoveryCloseIntentDirectory,
		v3RecoveryCloseSessionsDirectory,
		v3RecoveryCloseControlDirectory,
		v3RecoveryCloseCoordinatorLock,
		v3RecoveryClosePlatform,
	)
}

func v3RecoveryInstallDuplicateReservedRecords(
	t *testing.T,
	session *filesystemOutputSession,
	selection transfer.OutputSelection,
	object resumestate.OutputObjectID,
) (string, string) {
	t.Helper()
	type installedRecord struct {
		shard string
		name  string
	}
	records := make([]installedRecord, 0, len(selection.Files()))
	for index, selected := range selection.Files() {
		file := v3RecoveryOutputFileAt(t, session, selection, index)
		resumable, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
			Session: session.state, Descriptor: file.Descriptor,
			CanonicalLocator: selected.Path, OutputObject: object,
		})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := resumestate.EncodeFileRecord(resumable.Bound())
		if err != nil {
			t.Fatal(err)
		}
		name := resumestate.FileRecordName(resumable.Bound().Record().LocatorDigest())
		shard, _, err := openOutputShard(session.filesDir, name.Shard(), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.store.createRecord(shard, name.Name(), encoded, resumestate.MaxFileStateBytes); err != nil {
			_ = shard.Close()
			t.Fatal(err)
		}
		if err := shard.Close(); err != nil {
			t.Fatal(err)
		}
		records = append(records, installedRecord{shard: name.Shard(), name: name.Name()})
	}
	slices.SortFunc(records, func(left, right installedRecord) int {
		if left.shard != right.shard {
			if left.shard < right.shard {
				return -1
			}
			return 1
		}
		if left.name < right.name {
			return -1
		}
		if left.name > right.name {
			return 1
		}
		return 0
	})
	return records[0].shard, records[0].name
}

const (
	v3RecoveryCloseFiles             = "files"
	v3RecoveryCloseAnchors           = "anchors"
	v3RecoveryCloseStages            = "stages"
	v3RecoveryCloseSessionLock       = "session-lock"
	v3RecoveryCloseSessionDirectory  = "session-directory"
	v3RecoveryCloseIntentDirectory   = "intent-directory"
	v3RecoveryCloseSessionsDirectory = "sessions-directory"
	v3RecoveryCloseControlDirectory  = "control-directory"
	v3RecoveryCloseCoordinatorLock   = "coordinator-lock"
	v3RecoveryClosePlatform          = "platform"
)

type v3RecoveryInitializerCloseTracker struct {
	mu      sync.Mutex
	roles   map[string]string
	claimed map[string]bool
	count   map[string]int
}

func newV3RecoveryInitializerCloseTracker(
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
) *v3RecoveryInitializerCloseTracker {
	controlPath := filepath.Join(root, resumestate.ControlDirectoryName)
	sessionsPath := filepath.Join(controlPath, resumestate.SessionsDirectoryName)
	intentPath := filepath.Join(sessionsPath, resumestate.ResumeNamespaceName(selection.ResumeIntent()))
	sessionPath := filepath.Join(intentPath, resumestate.SessionDirectoryName(sessionID))
	return &v3RecoveryInitializerCloseTracker{
		roles: map[string]string{
			filepath.Clean(controlPath):                                  v3RecoveryCloseControlDirectory,
			filepath.Clean(sessionsPath):                                 v3RecoveryCloseSessionsDirectory,
			filepath.Clean(intentPath):                                   v3RecoveryCloseIntentDirectory,
			filepath.Clean(sessionPath):                                  v3RecoveryCloseSessionDirectory,
			filepath.Join(sessionPath, resumestate.FilesDirectoryName):   v3RecoveryCloseFiles,
			filepath.Join(sessionPath, resumestate.AnchorsDirectoryName): v3RecoveryCloseAnchors,
			filepath.Join(sessionPath, resumestate.StagesDirectoryName):  v3RecoveryCloseStages,
		},
		claimed: make(map[string]bool),
		count:   make(map[string]int),
	}
}

func (tracker *v3RecoveryInitializerCloseTracker) record(role string) {
	if role == "" {
		return
	}
	tracker.mu.Lock()
	tracker.count[role]++
	tracker.mu.Unlock()
}

func (tracker *v3RecoveryInitializerCloseTracker) claimRole(path string) string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	clean := filepath.Clean(path)
	role := tracker.roles[clean]
	if role == "" || tracker.claimed[clean] {
		return ""
	}
	tracker.claimed[clean] = true
	return role
}

func (tracker *v3RecoveryInitializerCloseTracker) assertExactlyOnce(t *testing.T, roles ...string) {
	t.Helper()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for _, role := range roles {
		if tracker.count[role] != 1 {
			t.Errorf("initializer close count for %s = %d, want 1", role, tracker.count[role])
		}
	}
}

type v3RecoveryInitializerFault struct {
	shardPath  string
	recordName string
}

type v3RecoveryInitializerPlatform struct {
	outputV3Platform
	root    outputV3Directory
	tracker *v3RecoveryInitializerCloseTracker
}

func v3RecoveryWrapInitializerPlatform(
	platform outputV3Platform,
	rootPath string,
	tracker *v3RecoveryInitializerCloseTracker,
	fault v3RecoveryInitializerFault,
) outputV3Platform {
	return &v3RecoveryInitializerPlatform{
		outputV3Platform: platform,
		root: v3RecoveryWrapInitializerDirectory(
			platform.Root(), rootPath, tracker, fault,
		),
		tracker: tracker,
	}
}

func (platform *v3RecoveryInitializerPlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryInitializerPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryInitializerDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapInitializerDirectory(
				root, decorated.path, decorated.tracker, decorated.fault,
			)
		},
	)
}

func (platform *v3RecoveryInitializerPlatform) Close() error {
	platform.tracker.record(v3RecoveryClosePlatform)
	return platform.outputV3Platform.Close()
}

type v3RecoveryInitializerDirectory struct {
	outputV3Directory
	path    string
	role    string
	tracker *v3RecoveryInitializerCloseTracker
	fault   v3RecoveryInitializerFault
}

func v3RecoveryWrapInitializerDirectory(
	directory outputV3Directory,
	path string,
	tracker *v3RecoveryInitializerCloseTracker,
	fault v3RecoveryInitializerFault,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	clean := filepath.Clean(path)
	if clean == filepath.Clean(fault.shardPath) {
		directory = &stateStoreFaultDirectory{
			outputV3Directory: directory,
			fault:             stateStoreFaultInstalledReopen,
			target:            fault.recordName,
		}
	}
	return &v3RecoveryInitializerDirectory{
		outputV3Directory: directory, path: clean, role: tracker.claimRole(clean), tracker: tracker, fault: fault,
	}
}

func v3RecoveryUnwrapInitializerDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryInitializerDirectory); ok {
		if faulted, ok := wrapped.outputV3Directory.(*stateStoreFaultDirectory); ok {
			return faulted.outputV3Directory
		}
		return wrapped.outputV3Directory
	}
	return directory
}

func (directory *v3RecoveryInitializerDirectory) Close() error {
	directory.tracker.record(directory.role)
	return directory.outputV3Directory.Close()
}

func (directory *v3RecoveryInitializerDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapInitializerDirectory(duplicate, directory.path, directory.tracker, directory.fault), err
}

func (directory *v3RecoveryInitializerDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapInitializerDirectory(other))
}

func (directory *v3RecoveryInitializerDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapInitializerDirectory(
		opened, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapInitializerDirectory(opened, "", directory.tracker, directory.fault), err
}

func (directory *v3RecoveryInitializerDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapInitializerDirectory(
		created, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapInitializerDirectory(candidate), name,
	)
	return v3RecoveryWrapInitializerDirectory(
		installed, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapInitializerDirectory(expected),
	)
}

func (directory *v3RecoveryInitializerDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	lock, created, err := directory.outputV3Directory.AcquireLock(name, existingOnly)
	if lock == nil {
		return nil, created, err
	}
	role := ""
	switch name {
	case resumestate.CoordinatorLockName:
		role = v3RecoveryCloseCoordinatorLock
	case resumestate.SessionLockName:
		role = v3RecoveryCloseSessionLock
	}
	return &v3RecoveryInitializerLock{outputV3Lock: lock, role: role, tracker: directory.tracker}, created, err
}

type v3RecoveryInitializerLock struct {
	outputV3Lock
	role    string
	tracker *v3RecoveryInitializerCloseTracker
}

func (lock *v3RecoveryInitializerLock) Close() error {
	lock.tracker.record(lock.role)
	return lock.outputV3Lock.Close()
}
