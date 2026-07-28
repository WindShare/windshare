package outputruntime

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
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
	reopenAuthority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
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
	session *Session,
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
		if _, err := session.store.CreateRecord(shard, name.Name(), encoded, resumestate.MaxFileStateBytes); err != nil {
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
	outputcap.Platform
	root    outputcap.Directory
	tracker *v3RecoveryInitializerCloseTracker
}

func v3RecoveryWrapInitializerPlatform(
	platform outputcap.Platform,
	rootPath string,
	tracker *v3RecoveryInitializerCloseTracker,
	fault v3RecoveryInitializerFault,
) outputcap.Platform {
	return &v3RecoveryInitializerPlatform{
		Platform: platform,
		root: v3RecoveryWrapInitializerDirectory(
			platform.Root(), rootPath, tracker, fault,
		),
		tracker: tracker,
	}
}

func (platform *v3RecoveryInitializerPlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryInitializerPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryInitializerDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return v3RecoveryWrapInitializerDirectory(
				root, decorated.path, decorated.tracker, decorated.fault,
			)
		},
	)
}

func (platform *v3RecoveryInitializerPlatform) Close() error {
	platform.tracker.record(v3RecoveryClosePlatform)
	return platform.Platform.Close()
}

type v3RecoveryInitializerDirectory struct {
	outputcap.Directory
	path    string
	role    string
	tracker *v3RecoveryInitializerCloseTracker
	fault   v3RecoveryInitializerFault
}

func v3RecoveryWrapInitializerDirectory(
	directory outputcap.Directory,
	path string,
	tracker *v3RecoveryInitializerCloseTracker,
	fault v3RecoveryInitializerFault,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	clean := filepath.Clean(path)
	if clean == filepath.Clean(fault.shardPath) {
		directory = &stateStoreFaultDirectory{
			Directory: directory,
			fault:     stateStoreFaultInstalledReopen,
			target:    fault.recordName,
		}
	}
	return &v3RecoveryInitializerDirectory{
		Directory: directory, path: clean, role: tracker.claimRole(clean), tracker: tracker, fault: fault,
	}
}

func v3RecoveryUnwrapInitializerDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryInitializerDirectory); ok {
		if faulted, ok := wrapped.Directory.(*stateStoreFaultDirectory); ok {
			return faulted.Directory
		}
		return wrapped.Directory
	}
	return directory
}

func (directory *v3RecoveryInitializerDirectory) Close() error {
	directory.tracker.record(directory.role)
	return directory.Directory.Close()
}

func (directory *v3RecoveryInitializerDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapInitializerDirectory(duplicate, directory.path, directory.tracker, directory.fault), err
}

func (directory *v3RecoveryInitializerDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapInitializerDirectory(other))
}

func (directory *v3RecoveryInitializerDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapInitializerDirectory(
		opened, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapInitializerDirectory(opened, "", directory.tracker, directory.fault), err
}

func (directory *v3RecoveryInitializerDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapInitializerDirectory(
		created, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapInitializerDirectory(candidate), name,
	)
	return v3RecoveryWrapInitializerDirectory(
		installed, filepath.Join(directory.path, name), directory.tracker, directory.fault,
	), err
}

func (directory *v3RecoveryInitializerDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(
		name, v3RecoveryUnwrapInitializerDirectory(expected),
	)
}

func (directory *v3RecoveryInitializerDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
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
	return &v3RecoveryInitializerLock{Lock: lock, role: role, tracker: directory.tracker}, created, err
}

type v3RecoveryInitializerLock struct {
	outputcap.Lock
	role    string
	tracker *v3RecoveryInitializerCloseTracker
}

func (lock *v3RecoveryInitializerLock) Close() error {
	lock.tracker.record(lock.role)
	return lock.Lock.Close()
}
