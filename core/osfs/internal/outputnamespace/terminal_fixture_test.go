package outputnamespace

import (
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

type terminalNamespaceFixture struct {
	platform          outputcap.Platform
	installedControl  *ControlNamespace
	control           *ControlNamespace
	sessionsDirectory *terminalFaultDirectory
	intentDirectory   *terminalFaultDirectory
	sessionDirectory  *terminalFaultDirectory
	layout            *TerminalLayout
	heldLock          outputcap.Lock
	header            resumestate.Header
	intentName        string
	sessionName       string
}

func newTerminalNamespaceFixture(
	t *testing.T,
	lifecycle resumestate.SessionLifecycle,
	inspect bool,
) *terminalNamespaceFixture {
	t.Helper()
	filesystem := v3RecoveryRoot(t)
	platform := filesystem.platform()
	controller := v3RecoveryAuthority(t, filesystem, nil)
	opened, err := controller.OpenOrBootstrapControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	selection := v3RecoverySelection(t, false, 0)
	intent, err := OpenCanonicalIntent(opened.Namespace.Sessions(), v3RecoveryIntentDigest(selection))
	if err != nil {
		t.Fatal(err)
	}
	root, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	session, err := controller.OpenOrCreateSession(
		intent, opened.Namespace.Control(), selection, v3RecoveryAncestryBinding(t, root, selection),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ReadRecord(session.Directory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		t.Fatal(err)
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil {
		t.Fatal(err)
	}
	intentName := resumestate.IntentNamespaceName(header.IntentDigest())
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	if lifecycle != resumestate.SessionActive {
		header = installTerminalLifecycle(
			t, controller, opened.Namespace.Control(), intentName, sessionName, session.Directory, header, lifecycle,
		)
	}
	sessionsFault := &terminalFaultDirectory{Directory: opened.Namespace.Sessions()}
	intentFault := &terminalFaultDirectory{Directory: intent}
	sessionFault := &terminalFaultDirectory{Directory: session.Directory}
	control := *opened.Namespace
	control.sessions = sessionsFault
	fixture := &terminalNamespaceFixture{
		platform: platform, installedControl: opened.Namespace, control: &control,
		sessionsDirectory: sessionsFault, intentDirectory: intentFault, sessionDirectory: sessionFault,
		header: header, intentName: intentName, sessionName: sessionName,
	}
	if inspect {
		fixture.layout, err = InspectTerminalLayout(sessionFault, header, nil)
		if err != nil {
			t.Fatal(err)
		}
		fixture.layout.stages = &terminalFaultDirectory{Directory: fixture.layout.stages}
		fixture.layout.anchors = &terminalFaultDirectory{Directory: fixture.layout.anchors}
		fixture.layout.files = &terminalFaultDirectory{Directory: fixture.layout.files}
	}
	return fixture
}

func installTerminalLifecycle(
	t *testing.T,
	controller Controller,
	control resumestate.Control,
	intentName string,
	sessionName string,
	directory outputcap.Directory,
	header resumestate.Header,
	lifecycle resumestate.SessionLifecycle,
) resumestate.Header {
	t.Helper()
	namespace, err := resumestate.BindSessionNamespaceAuthority(control, header, intentName, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	next, err := namespace.WithLifecycle(lifecycle)
	if err != nil {
		t.Fatal(err)
	}
	currentEncoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	nextEncoded, err := resumestate.EncodeHeader(next.Header())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := controller.Store(header.IntentDigest(), header.SessionID()).ReplaceRecord(
		directory,
		resumestate.HeaderRecordName,
		NewRecordImage(currentEncoded, header.StateGeneration()),
		NewRecordImage(nextEncoded, next.Header().StateGeneration()),
		resumestate.MaxSessionHeaderBytes,
	)
	if err != nil || outcome != ReplaceAdopted {
		t.Fatalf("install terminal lifecycle = (%v, %v)", outcome, err)
	}
	return next.Header()
}

func (fixture *terminalNamespaceFixture) close(t *testing.T) {
	t.Helper()
	if err := errors.Join(
		closeLock(fixture.heldLock),
		fixture.layout.Close(),
		fixture.sessionDirectory.Close(),
		fixture.intentDirectory.Close(),
		fixture.installedControl.Close(),
		fixture.platform.Close(),
	); err != nil {
		t.Errorf("close terminal fixture: %v", err)
	}
}

func assertTerminalCut(
	t *testing.T,
	fixture *terminalNamespaceFixture,
	removedEntries int,
	wantSession bool,
	wantIntent bool,
) {
	t.Helper()
	intentKind, err := fixture.installedControl.Sessions().ObserveEntry(fixture.intentName)
	if err != nil {
		t.Fatal(err)
	}
	wantIntentKind := outputcap.EntryAbsent
	if wantIntent {
		wantIntentKind = outputcap.EntryDirectory
	}
	if intentKind != wantIntentKind {
		t.Fatalf("intent kind = %v, want %v", intentKind, wantIntentKind)
	}
	if !wantIntent {
		return
	}
	sessionKind, err := fixture.intentDirectory.ObserveEntry(fixture.sessionName)
	if err != nil {
		t.Fatal(err)
	}
	wantSessionKind := outputcap.EntryAbsent
	if wantSession {
		wantSessionKind = outputcap.EntryDirectory
	}
	if sessionKind != wantSessionKind {
		t.Fatalf("session kind = %v, want %v", sessionKind, wantSessionKind)
	}
	if !wantSession {
		return
	}
	for index, name := range terminalRemovalOrder {
		kind, err := fixture.sessionDirectory.ObserveEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		want := outputcap.EntryRegularFile
		if index < 3 {
			want = outputcap.EntryDirectory
		}
		if index < removedEntries {
			want = outputcap.EntryAbsent
		}
		if kind != want {
			t.Fatalf("terminal entry %q at cut %d = %v, want %v", name, removedEntries, kind, want)
		}
	}
}

var errTerminalInjected = errors.New("injected terminal recovery failure")

type terminalFaultDirectory struct {
	outputcap.Directory
	namesErr            error
	openFileName        string
	openFileErr         error
	forceOpenFileErr    bool
	readFileErr         error
	forceCreatedLock    bool
	removeDirectoryName string
	removeFileName      string
	syncErrAt           int
	syncCalls           int
	closeErr            error
	closed              bool
}

func (directory *terminalFaultDirectory) Names(limit int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	return directory.Directory.Names(limit)
}

func (directory *terminalFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if name == directory.openFileName && directory.forceOpenFileErr {
		return nil, errTerminalInjected
	}
	if name == directory.openFileName && directory.openFileErr != nil {
		return nil, directory.openFileErr
	}
	file, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil || name != directory.openFileName || directory.readFileErr == nil {
		return file, err
	}
	return &terminalFaultFile{File: file, readErr: directory.readFileErr}, nil
}

func (directory *terminalFaultDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
	if err == nil && directory.forceCreatedLock {
		created = true
	}
	return lock, created, err
}

func (directory *terminalFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapTerminalDirectory(other))
}

func (directory *terminalFaultDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	if name == directory.removeDirectoryName {
		return errTerminalInjected
	}
	return directory.Directory.RemoveDirectory(name, unwrapTerminalDirectory(expected))
}

func (directory *terminalFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if name == directory.removeFileName {
		return errTerminalInjected
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *terminalFaultDirectory) Sync() error {
	directory.syncCalls++
	if directory.syncErrAt > 0 && directory.syncCalls == directory.syncErrAt {
		return errTerminalInjected
	}
	return directory.Directory.Sync()
}

func (directory *terminalFaultDirectory) Close() error {
	if directory == nil || directory.closed {
		return nil
	}
	directory.closed = true
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

type terminalFaultFile struct {
	outputcap.File
	readErr error
}

func (file *terminalFaultFile) ReadAt([]byte, int64) (int, error) { return 0, file.readErr }

type terminalFaultLock struct {
	outputcap.Lock
	nilFile  bool
	closeErr error
	closed   bool
}

func (lock *terminalFaultLock) File() outputcap.File {
	if lock.nilFile {
		return nil
	}
	return lock.Lock.File()
}

func (lock *terminalFaultLock) Close() error {
	if lock == nil || lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(lock.Lock.Close(), lock.closeErr)
}

type terminalAuthorityDirectory struct {
	outputcap.Directory
	openCalls int
	failAt    int
}

func (directory *terminalAuthorityDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	directory.openCalls++
	if directory.openCalls == directory.failAt {
		return nil, errTerminalInjected
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *terminalAuthorityDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapTerminalDirectory(other))
}

func unwrapTerminalDirectory(directory outputcap.Directory) outputcap.Directory {
	switch wrapped := directory.(type) {
	case *terminalFaultDirectory:
		return wrapped.Directory
	case *terminalAuthorityDirectory:
		return wrapped.Directory
	default:
		return directory
	}
}

type memoryCapabilityFile struct {
	filesystem *memoryCapabilityFS
	node       *memoryCapabilityNode
	closed     bool
}

func (file *memoryCapabilityFile) ReadAt(target []byte, offset int64) (int, error) {
	if err := file.usable(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, outputcap.ErrUnsafeNamespace
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	if offset >= int64(len(file.node.data)) {
		return 0, io.EOF
	}
	read := copy(target, file.node.data[int(offset):])
	if read != len(target) {
		return read, io.EOF
	}
	return read, nil
}

func (file *memoryCapabilityFile) WriteAt(source []byte, offset int64) (int, error) {
	if err := file.usable(); err != nil {
		return 0, err
	}
	if offset < 0 || offset > int64(int(^uint(0)>>1))-int64(len(source)) {
		return 0, outputcap.ErrUnsafeNamespace
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	end := int(offset) + len(source)
	if end > len(file.node.data) {
		file.node.data = append(file.node.data, make([]byte, end-len(file.node.data))...)
	}
	copy(file.node.data[int(offset):], source)
	return len(source), nil
}

func (file *memoryCapabilityFile) Close() error {
	if file == nil {
		return nil
	}
	file.closed = true
	return nil
}

func (file *memoryCapabilityFile) Sync() error { return file.usable() }

func (file *memoryCapabilityFile) Truncate(size int64) error {
	if err := file.usable(); err != nil {
		return err
	}
	if size < 0 || size > int64(int(^uint(0)>>1)) {
		return outputcap.ErrUnsafeNamespace
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	if int(size) <= len(file.node.data) {
		file.node.data = file.node.data[:int(size)]
	} else {
		file.node.data = append(file.node.data, make([]byte, int(size)-len(file.node.data))...)
	}
	return nil
}

func (file *memoryCapabilityFile) Size() (uint64, error) {
	if err := file.usable(); err != nil {
		return 0, err
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	return uint64(len(file.node.data)), nil
}

func (file *memoryCapabilityFile) AllocatedSize() (uint64, error) { return file.Size() }

func (file *memoryCapabilityFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if err := file.usable(); err != nil {
		return err
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	file.node.modified = modified
	return nil
}

func (file *memoryCapabilityFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if err := file.usable(); err != nil {
		return false, err
	}
	file.filesystem.mu.Lock()
	defer file.filesystem.mu.Unlock()
	return uint64(len(file.node.data)) == size && file.node.modified == modified, nil
}

func (file *memoryCapabilityFile) SameFile(other outputcap.File) (bool, error) {
	if err := file.usable(); err != nil {
		return false, err
	}
	node, err := fileNode(other)
	return node == file.node, err
}

func (file *memoryCapabilityFile) usable() error {
	if file == nil || file.filesystem == nil || file.node == nil || file.closed {
		return fs.ErrClosed
	}
	if file.node.kind != outputcap.EntryRegularFile {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func fileNode(file outputcap.File) (*memoryCapabilityNode, error) {
	capability, ok := file.(*memoryCapabilityFile)
	if !ok || capability.node == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return capability.node, nil
}

type memoryCapabilityEntryReference struct {
	filesystem *memoryCapabilityFS
	node       *memoryCapabilityNode
	closed     bool
}

func (reference *memoryCapabilityEntryReference) Kind() outputcap.EntryKind {
	if reference == nil || reference.node == nil {
		return outputcap.EntryAbsent
	}
	return reference.node.kind
}

func (reference *memoryCapabilityEntryReference) AllocatedSize() (uint64, error) {
	if reference == nil || reference.node == nil || reference.closed {
		return 0, fs.ErrClosed
	}
	reference.filesystem.mu.Lock()
	defer reference.filesystem.mu.Unlock()
	return uint64(len(reference.node.data)), nil
}

func (reference *memoryCapabilityEntryReference) Close() error {
	if reference != nil {
		reference.closed = true
	}
	return nil
}

type memoryCapabilityLock struct {
	filesystem *memoryCapabilityFS
	node       *memoryCapabilityNode
	file       outputcap.File
	closed     bool
}

func (lock *memoryCapabilityLock) File() outputcap.File {
	if lock == nil {
		return nil
	}
	return lock.file
}

func (lock *memoryCapabilityLock) Close() error {
	if lock == nil || lock.closed {
		return nil
	}
	lock.filesystem.mu.Lock()
	lock.node.locked = false
	lock.filesystem.mu.Unlock()
	lock.closed = true
	return lock.file.Close()
}

func TestOutputV3HeaderTemporaryRecoveryChecksAuthorityBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		verifyFault bool
		removeFault bool
		syncFault   bool
		wantPresent bool
	}{
		{name: "authority-changed", verifyFault: true, wantPresent: true},
		{name: "remove-failed", removeFault: true, wantPresent: true},
		{name: "sync-failed-after-remove", syncFault: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := newTestStateSession(t, v3RecoverySelection(t, false, 0))
			defer session.close(t)
			temporaryName, err := session.store.temporaryName(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := resumestate.EncodeHeader(session.state.Header())
			if err != nil {
				t.Fatal(err)
			}
			writeStateStoreHeaderTemporary(t, session.sessionDir, temporaryName, encoded)

			faults := &stateStoreReconcileFaultDirectory{Directory: session.sessionDir}
			if test.removeFault {
				faults.removeErr = errStateStoreInjected
			}
			if test.syncFault {
				faults.syncErr = errStateStoreInjected
			}
			verifyCalls := 0
			verify := func() error {
				verifyCalls++
				if test.verifyFault && verifyCalls == 2 {
					return errStateStoreInjected
				}
				return nil
			}
			if err := ReconcileHeaderRecordTemporaries(
				faults, session.state.NamespaceAuthority(), verify,
			); !errors.Is(err, errStateStoreInjected) {
				t.Fatalf("reconcile fault error = %v, want injected failure", err)
			}
			kind, err := session.sessionDir.ObserveEntry(temporaryName)
			if err != nil {
				t.Fatal(err)
			}
			present := kind != outputcap.EntryAbsent
			if present != test.wantPresent {
				t.Fatalf("temporary present=%t after failed cut, want %t", present, test.wantPresent)
			}
		})
	}
}
