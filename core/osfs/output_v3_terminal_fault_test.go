package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3TerminalRecoveryFailureCutsRemainRestartable(t *testing.T) {
	for _, test := range []struct {
		name               string
		lifecycle          resumestate.SessionLifecycle
		discard            bool
		configure          func(*testing.T, *terminalRecoveryFaultFixture)
		wantRemovedEntries int
		wantSession        bool
		wantIntent         bool
		wantInjected       bool
	}{
		{
			name: "discard-child-enumeration", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.layout.stages.(*terminalFaultDirectory).namesErr = errTerminalRecoveryInjected
			},
			wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "completing-child-not-empty", lifecycle: resumestate.SessionCompleting,
			configure: func(t *testing.T, fixture *terminalRecoveryFaultFixture) {
				file, err := fixture.layout.stages.CreateFile("unexpected", true, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), fixture.layout.stages.Sync(), file.Close()); err != nil {
					t.Fatal(err)
				}
			},
			wantSession: true, wantIntent: true,
		},
		{
			name: "remove-stage", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.removeDirectoryName = resumestate.StagesDirectoryName
			},
			wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-stage", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 1, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "close-stage", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.layout.stages.(*terminalFaultDirectory).closeErr = errTerminalRecoveryInjected
			},
			wantRemovedEntries: 1, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "missing-lock-file-authority", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.layout.lock = &terminalFaultLock{outputV3Lock: fixture.layout.lock, nilFile: true}
			},
			wantRemovedEntries: 3, wantSession: true, wantIntent: true,
		},
		{
			name: "remove-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.removeFileName = resumestate.SessionLockName
			},
			wantRemovedEntries: 3, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.syncErrAt = 4
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "close-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.layout.lock = &terminalFaultLock{outputV3Lock: fixture.layout.lock, closeErr: errTerminalRecoveryInjected}
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-header", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.removeFileName = resumestate.HeaderRecordName
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-header", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.syncErrAt = 5
			},
			wantRemovedEntries: 5, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-session-shell", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.intentDirectory.removeDirectoryName = fixture.sessionName
			},
			wantRemovedEntries: 5, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-intent-after-session-removal", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.intentDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 5, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-empty-intent", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionsDirectory.removeDirectoryName = fixture.intentName
			},
			wantRemovedEntries: 5, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-sessions-after-intent-removal", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionsDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 5, wantInjected: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTerminalRecoveryFaultFixture(t, test.lifecycle)
			defer fixture.close(t)
			test.configure(t, fixture)

			err := recoverTerminalNamespace(
				fixture.control,
				fixture.intentDirectory,
				fixture.sessionDirectory,
				fixture.header,
				fixture.layout,
				test.discard,
			)
			if err == nil {
				t.Fatal("injected terminal cut unexpectedly completed")
			}
			if test.wantInjected && !errors.Is(err, errTerminalRecoveryInjected) {
				t.Fatalf("terminal cut error = %v, want injected failure", err)
			}
			assertTerminalRecoveryCut(
				t, fixture, test.wantRemovedEntries, test.wantSession, test.wantIntent,
			)
		})
	}
}

func TestOutputV3TerminalLayoutInspectionRejectsAmbiguousOrUnownedCuts(t *testing.T) {
	for _, test := range []struct {
		name             string
		prepare          func(*testing.T, *filesystemOutputSession, string)
		headerOpenFault  bool
		forceCreatedLock bool
	}{
		{name: "live-lock"},
		{name: "header-reopen-failed", headerOpenFault: true},
		{
			name: "enumerated-lock-was-recreated",
			prepare: func(t *testing.T, session *filesystemOutputSession, _ string) {
				if err := session.sessionLock.Close(); err != nil {
					t.Fatal(err)
				}
				session.sessionLock = nil
			},
			forceCreatedLock: true,
		},
		{
			name: "unexpected-entry",
			prepare: func(t *testing.T, session *filesystemOutputSession, _ string) {
				file, err := session.sessionDir.CreateFile("unexpected", true, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), session.sessionDir.Sync(), file.Close()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt-header",
			prepare: func(t *testing.T, _ *filesystemOutputSession, sessionPath string) {
				if err := os.WriteFile(filepath.Join(sessionPath, resumestate.HeaderRecordName), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stage-is-not-directory",
			prepare: func(t *testing.T, session *filesystemOutputSession, sessionPath string) {
				if err := session.stagesDir.Close(); err != nil {
					t.Fatal(err)
				}
				session.stagesDir = nil
				stagePath := filepath.Join(sessionPath, resumestate.StagesDirectoryName)
				if err := os.Remove(stagePath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stagePath, []byte("not-a-directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonempty-lock-record",
			prepare: func(t *testing.T, session *filesystemOutputSession, sessionPath string) {
				if err := session.sessionLock.Close(); err != nil {
					t.Fatal(err)
				}
				session.sessionLock = nil
				if err := os.WriteFile(filepath.Join(sessionPath, resumestate.SessionLockName), []byte{1}, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
			defer v3RecoveryCloseSession(t, session)
			if err := session.installLifecycle(resumestate.SessionCompleting); err != nil {
				t.Fatal(err)
			}
			sessionPath := v3RecoverySessionPath(root, selection, session.SessionID())
			if test.prepare != nil {
				test.prepare(t, session, sessionPath)
			}

			directory := outputV3Directory(session.sessionDir)
			if test.headerOpenFault || test.forceCreatedLock {
				directory = &terminalFaultDirectory{
					outputV3Directory: session.sessionDir,
					openFileName:      resumestate.HeaderRecordName,
					forceOpenFileErr:  test.headerOpenFault,
					forceCreatedLock:  test.forceCreatedLock,
				}
			}
			layout, err := inspectTerminalSessionLayout(directory, session.state.Header())
			if layout != nil {
				_ = layout.close()
			}
			if err == nil {
				t.Fatal("ambiguous or owned terminal cut unexpectedly accepted")
			}
		})
	}
}

func TestOutputV3TerminalAuthorityVerificationRejectsStaleBindings(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)
	header := session.state.Header()
	next, err := session.state.NamespaceAuthority().WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		intent    outputV3Directory
		session   outputV3Directory
		header    resumestate.Header
		wantError bool
	}{
		{
			name: "current-authority", intent: session.intentDir,
			session: session.sessionDir, header: header,
		},
		{
			name: "intent-entry-rebound", intent: session.sessionDir,
			session: session.sessionDir, header: header, wantError: true,
		},
		{
			name: "session-entry-rebound", intent: session.intentDir,
			session: session.stagesDir, header: header, wantError: true,
		},
		{
			name: "stale-header-generation", intent: session.intentDir,
			session: session.sessionDir, header: next.Header(), wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyTerminalSessionAuthority(
				session.control, test.intent, test.session, test.header,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("terminal authority verification error = %v, want error=%t", err, test.wantError)
			}
		})
	}
}

func TestOutputV3TerminalHeaderRemovalRequiresExactReopenedImage(t *testing.T) {
	for _, test := range []struct {
		name          string
		zeroExpected  bool
		staleExpected bool
		openErr       error
		readErr       error
		wantInjected  bool
	}{
		{name: "invalid-expected-header", zeroExpected: true},
		{name: "stale-expected-header", staleExpected: true},
		{name: "header-reopen-failed", openErr: errTerminalRecoveryInjected, wantInjected: true},
		{name: "header-reread-failed", readErr: errTerminalRecoveryInjected, wantInjected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
			defer v3RecoveryCloseSession(t, session)
			expected := session.state.Header()
			if test.zeroExpected {
				expected = resumestate.Header{}
			}
			if test.staleExpected {
				next, err := session.state.NamespaceAuthority().WithLifecycle(resumestate.SessionPausing)
				if err != nil {
					t.Fatal(err)
				}
				expected = next.Header()
			}
			directory := &terminalFaultDirectory{
				outputV3Directory: session.sessionDir,
				openFileName:      resumestate.HeaderRecordName,
				openFileErr:       test.openErr,
				readFileErr:       test.readErr,
			}
			err := removeTerminalHeader(directory, expected)
			if err == nil {
				t.Fatal("terminal header removal accepted unverified image")
			}
			if test.wantInjected && !errors.Is(err, errTerminalRecoveryInjected) {
				t.Fatalf("terminal header removal error = %v, want injected failure", err)
			}
			if kind, observeErr := session.sessionDir.ObserveEntry(resumestate.HeaderRecordName); observeErr != nil || kind != outputV3EntryRegularFile {
				t.Fatalf("header after rejected removal = (%v, %v), want regular file", kind, observeErr)
			}
		})
	}
}

func TestOutputV3TerminalInspectionAndShellRemovalFailBeforeMutation(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)

	directory := &terminalFaultDirectory{
		outputV3Directory: session.sessionDir,
		namesErr:          errTerminalRecoveryInjected,
	}
	if layout, err := inspectTerminalSessionLayout(directory, session.state.Header()); layout != nil || !errors.Is(err, errTerminalRecoveryInjected) {
		t.Fatalf("terminal layout enumeration = (%v, %v), want nil and injected failure", layout, err)
	}
	intentName := resumestate.ResumeNamespaceName(session.state.Header().ResumeIntent())
	sessionName := resumestate.SessionDirectoryName(session.SessionID())
	if err := removeEmptySessionShell(
		session.control.sessions, session.intentDir, session.sessionDir, intentName, sessionName,
	); err == nil {
		t.Fatal("non-empty session shell was removed")
	}
	if kind, err := session.intentDir.ObserveEntry(sessionName); err != nil || kind != outputV3EntryDirectory {
		t.Fatalf("session shell after rejected removal = (%v, %v), want directory", kind, err)
	}
}

func TestOutputV3TerminalSessionRecoveryPropagatesPreflightFailuresBeforeRetirement(t *testing.T) {
	for _, test := range []struct {
		name         string
		configure    func(*testing.T, *terminalRecoveryFaultFixture)
		wrongIntent  bool
		wantInjected bool
	}{
		{
			name: "layout-inspection-failed",
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.namesErr = errTerminalRecoveryInjected
			},
			wantInjected: true,
		},
		{name: "intent-authority-changed", wrongIntent: true},
		{
			name: "settled-file-namespace-rescan-failed",
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.childNamesErrAt = map[string]int{
					resumestate.FilesDirectoryName: 2,
				}
			},
			wantInjected: true,
		},
		{
			name: "empty-shard-inspection-failed",
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.sessionDirectory.childNamesErrAt = map[string]int{
					resumestate.StagesDirectoryName: 1,
				}
			},
			wantInjected: true,
		},
		{
			name: "attention-lifecycle-install-failed",
			configure: func(t *testing.T, fixture *terminalRecoveryFaultFixture) {
				unexpected, err := fixture.layout.stages.CreateFile("unexpected", true, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(unexpected.Sync(), fixture.layout.stages.Sync(), unexpected.Close()); err != nil {
					t.Fatal(err)
				}
				fixture.sessionDirectory.replaceErr = errTerminalRecoveryInjected
			},
			wantInjected: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionCompleting)
			defer fixture.close(t)
			if test.configure != nil {
				test.configure(t, fixture)
			}
			if err := fixture.layout.close(); err != nil {
				t.Fatal(err)
			}
			fixture.layout = &outputTerminalLayout{}
			intentDirectory := outputV3Directory(fixture.intentDirectory)
			if test.wrongIntent {
				intentDirectory = fixture.sessionDirectory
			}
			attention, err := fixture.session.owner.recoverTerminalSession(
				fixture.session.platform,
				fixture.control,
				intentDirectory,
				fixture.sessionDirectory,
				fixture.session.state,
				outputSelectionAdmission{
					selection: fixture.session.selection,
					files:     fixture.session.selectedFiles,
					dirs:      fixture.session.selectedDirs,
				},
			)
			if attention || err == nil {
				t.Fatalf("terminal recovery preflight = (attention=%t, %v), want false and error", attention, err)
			}
			if test.wantInjected && !errors.Is(err, errTerminalRecoveryInjected) {
				t.Fatalf("terminal recovery preflight error = %v, want injected failure", err)
			}
			assertTerminalRecoveryCut(t, fixture, 0, true, true)
		})
	}
}

func TestOutputV3DiscardHeaderStopsAfterAdoptedFixedReopenCloseFailure(t *testing.T) {
	fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionActive)
	defer fixture.close(t)
	fixture.sessionDirectory.openFileName = resumestate.HeaderRecordName
	fixture.sessionDirectory.openFileCloseErrAt = 4
	reference := ResumeStateRef{
		kind:   ResumeStateRecoverable,
		intent: fixture.header.ResumeIntent(), session: fixture.header.SessionID(),
		namespaceName: fixture.intentName, sessionName: fixture.sessionName,
	}

	namespace, removable, corrupt, err := installDiscardingHeader(
		fixture.session.store, fixture.control.control, fixture.sessionDirectory,
		reference, true, func() error { return nil },
	)
	if !errors.Is(err, errTerminalRecoveryInjected) || removable || corrupt || namespace.Header().StateGeneration() != 0 {
		t.Fatalf("discard header close cut = (removable=%t, corrupt=%t, lifecycle=%v, err=%v)",
			removable, corrupt, namespace.Header().Lifecycle(), err)
	}
	encoded, readErr := readStateRecord(
		fixture.sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes,
	)
	header, decodeErr := resumestate.DecodeHeader(encoded)
	if readErr != nil || decodeErr != nil || header.Lifecycle() != resumestate.SessionDiscarding {
		t.Fatalf("adopted discard header = (lifecycle=%v, read=%v, decode=%v)",
			header.Lifecycle(), readErr, decodeErr)
	}
	for _, name := range []string{
		resumestate.FilesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.StagesDirectoryName,
	} {
		kind, observeErr := fixture.sessionDirectory.ObserveEntry(name)
		if observeErr != nil || kind != outputV3EntryDirectory {
			t.Fatalf("discard close cut child %q = (kind=%v, err=%v)", name, kind, observeErr)
		}
	}
}

type terminalRecoveryFaultFixture struct {
	session           *filesystemOutputSession
	control           *outputControlNamespace
	sessionsDirectory *terminalFaultDirectory
	intentDirectory   *terminalFaultDirectory
	sessionDirectory  *terminalFaultDirectory
	layout            *outputTerminalLayout
	header            resumestate.Header
	intentName        string
	sessionName       string
}

func newTerminalRecoveryFaultFixture(
	t *testing.T,
	lifecycle resumestate.SessionLifecycle,
) *terminalRecoveryFaultFixture {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	if lifecycle != resumestate.SessionActive {
		if err := session.installLifecycle(lifecycle); err != nil {
			v3RecoveryCloseSession(t, session)
			t.Fatal(err)
		}
	}

	session.mu.Lock()
	layout := &outputTerminalLayout{
		stages:  &terminalFaultDirectory{outputV3Directory: session.stagesDir},
		anchors: &terminalFaultDirectory{outputV3Directory: session.anchorsDir},
		files:   &terminalFaultDirectory{outputV3Directory: session.filesDir},
		lock:    session.sessionLock,
	}
	session.stagesDir, session.anchorsDir, session.filesDir, session.sessionLock = nil, nil, nil, nil
	session.mu.Unlock()

	sessionsDirectory := &terminalFaultDirectory{outputV3Directory: session.control.sessions}
	intentDirectory := &terminalFaultDirectory{outputV3Directory: session.intentDir}
	sessionDirectory := &terminalFaultDirectory{outputV3Directory: session.sessionDir}
	control := *session.control
	control.sessions = sessionsDirectory
	header := session.state.Header()
	return &terminalRecoveryFaultFixture{
		session: session, control: &control,
		sessionsDirectory: sessionsDirectory,
		intentDirectory:   intentDirectory,
		sessionDirectory:  sessionDirectory,
		layout:            layout, header: header,
		intentName:  resumestate.ResumeNamespaceName(header.ResumeIntent()),
		sessionName: resumestate.SessionDirectoryName(header.SessionID()),
	}
}

func (fixture *terminalRecoveryFaultFixture) close(t *testing.T) {
	t.Helper()
	if err := errors.Join(fixture.layout.close(), fixture.session.closeHandles()); err != nil {
		t.Errorf("close terminal fault fixture: %v", err)
	}
}

func assertTerminalRecoveryCut(
	t *testing.T,
	fixture *terminalRecoveryFaultFixture,
	removedEntries int,
	wantSession bool,
	wantIntent bool,
) {
	t.Helper()
	intentKind, err := fixture.session.control.sessions.ObserveEntry(fixture.intentName)
	if err != nil {
		t.Fatal(err)
	}
	wantIntentKind := outputV3EntryAbsent
	if wantIntent {
		wantIntentKind = outputV3EntryDirectory
	}
	if intentKind != wantIntentKind {
		t.Fatalf("intent kind = %v, want %v", intentKind, wantIntentKind)
	}
	if !wantIntent {
		return
	}
	sessionKind, err := fixture.session.intentDir.ObserveEntry(fixture.sessionName)
	if err != nil {
		t.Fatal(err)
	}
	wantSessionKind := outputV3EntryAbsent
	if wantSession {
		wantSessionKind = outputV3EntryDirectory
	}
	if sessionKind != wantSessionKind {
		t.Fatalf("session kind = %v, want %v", sessionKind, wantSessionKind)
	}
	if !wantSession {
		return
	}
	for index, name := range outputTerminalRemovalOrder {
		kind, err := fixture.session.sessionDir.ObserveEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		want := outputV3EntryRegularFile
		if index < 3 {
			want = outputV3EntryDirectory
		}
		if index < removedEntries {
			want = outputV3EntryAbsent
		}
		if kind != want {
			t.Fatalf("terminal entry %q at cut %d = %v, want %v", name, removedEntries, kind, want)
		}
	}
}

var errTerminalRecoveryInjected = errors.New("injected terminal recovery failure")

type terminalFaultDirectory struct {
	outputV3Directory
	namesErr            error
	namesErrAt          int
	namesCalls          int
	childNamesErrAt     map[string]int
	openFileName        string
	openFileErr         error
	forceOpenFileErr    bool
	readFileErr         error
	openFileCloseErrAt  int
	openFileCalls       int
	replaceErr          error
	forceCreatedLock    bool
	removeDirectoryName string
	removeFileName      string
	syncErrAt           int
	syncCalls           int
	closeErr            error
	closed              bool
}

func (directory *terminalFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputV3File, error) {
	file, err := directory.outputV3Directory.CreateFile(name, private, size)
	if err != nil {
		return file, err
	}
	return file, nil
}

func (directory *terminalFaultDirectory) Names(limit int) ([]string, error) {
	directory.namesCalls++
	if directory.namesErrAt > 0 && directory.namesCalls == directory.namesErrAt {
		return nil, errTerminalRecoveryInjected
	}
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	return directory.outputV3Directory.Names(limit)
}

func (directory *terminalFaultDirectory) OpenDirectory(name string, private bool) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &terminalFaultDirectory{
		outputV3Directory: opened,
		namesErrAt:        directory.childNamesErrAt[name],
	}, nil
}

func (directory *terminalFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	matched := name == directory.openFileName
	if matched {
		directory.openFileCalls++
	}
	if name == directory.openFileName && directory.forceOpenFileErr {
		return nil, errTerminalRecoveryInjected
	}
	if name == directory.openFileName && directory.openFileErr != nil {
		return nil, directory.openFileErr
	}
	file, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	closeErr := error(nil)
	if matched && directory.openFileCalls == directory.openFileCloseErrAt {
		closeErr = errTerminalRecoveryInjected
	}
	if matched && (directory.readFileErr != nil || closeErr != nil) {
		return &terminalFaultFile{
			outputV3File: file, readErr: directory.readFileErr, closeErr: closeErr,
		}, nil
	}
	return file, nil
}

func (directory *terminalFaultDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	lock, created, err := directory.outputV3Directory.AcquireLock(name, existingOnly)
	if err == nil && directory.forceCreatedLock {
		created = true
	}
	return lock, created, err
}

func (directory *terminalFaultDirectory) ReplacePrivateFile(source outputV3File, name string) error {
	if directory.replaceErr != nil {
		return directory.replaceErr
	}
	if wrapped, ok := source.(*terminalFaultFile); ok {
		source = wrapped.outputV3File
	}
	return directory.outputV3Directory.ReplacePrivateFile(source, name)
}

func (directory *terminalFaultDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(unwrapTerminalFaultDirectory(other))
}

func (directory *terminalFaultDirectory) RemoveDirectory(name string, expected outputV3Directory) error {
	if name == directory.removeDirectoryName {
		return errTerminalRecoveryInjected
	}
	return directory.outputV3Directory.RemoveDirectory(name, unwrapTerminalFaultDirectory(expected))
}

func (directory *terminalFaultDirectory) RemoveFile(name string, expected outputV3File) error {
	if name == directory.removeFileName {
		return errTerminalRecoveryInjected
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *terminalFaultDirectory) Sync() error {
	directory.syncCalls++
	if directory.syncErrAt > 0 && directory.syncCalls == directory.syncErrAt {
		return errTerminalRecoveryInjected
	}
	return directory.outputV3Directory.Sync()
}

func (directory *terminalFaultDirectory) Close() error {
	if directory.closed {
		return nil
	}
	directory.closed = true
	return errors.Join(directory.outputV3Directory.Close(), directory.closeErr)
}

func unwrapTerminalFaultDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*terminalFaultDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

type terminalFaultFile struct {
	outputV3File
	readErr  error
	closeErr error
}

func (file *terminalFaultFile) ReadAt(data []byte, offset int64) (int, error) {
	if file.readErr != nil {
		return 0, file.readErr
	}
	return file.outputV3File.ReadAt(data, offset)
}

func (file *terminalFaultFile) Close() error {
	return errors.Join(file.outputV3File.Close(), file.closeErr)
}

type terminalFaultLock struct {
	outputV3Lock
	nilFile  bool
	closeErr error
	closed   bool
}

func (lock *terminalFaultLock) File() outputV3File {
	if lock.nilFile {
		return nil
	}
	return lock.outputV3Lock.File()
}

func (lock *terminalFaultLock) Close() error {
	if lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(lock.outputV3Lock.Close(), lock.closeErr)
}
