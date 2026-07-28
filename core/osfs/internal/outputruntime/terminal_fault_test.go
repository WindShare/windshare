package outputruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3TerminalRecoveryFailureCutsRemainRestartable(t *testing.T) {
	t.Parallel()
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
				fixture.stages.namesErr = errTerminalRecoveryInjected
			},
			wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "completing-child-not-empty", lifecycle: resumestate.SessionCompleting,
			configure: func(t *testing.T, fixture *terminalRecoveryFaultFixture) {
				file, err := fixture.stages.CreateFile("unexpected", true, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), fixture.stages.Sync(), file.Close()); err != nil {
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
				fixture.stages.closeErr = errTerminalRecoveryInjected
			},
			wantRemovedEntries: 1, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "missing-lock-file-authority", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalRecoveryFaultFixture) {
				fixture.lock.nilFile = true
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
				fixture.lock.closeErr = errTerminalRecoveryInjected
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

			err := outputnamespace.RecoverTerminalNamespace(
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
	t.Parallel()
	for _, test := range []struct {
		name             string
		prepare          func(*testing.T, *Session, string)
		headerOpenFault  bool
		forceCreatedLock bool
	}{
		{name: "live-lock"},
		{name: "header-reopen-failed", headerOpenFault: true},
		{
			name: "enumerated-lock-was-recreated",
			prepare: func(t *testing.T, session *Session, _ string) {
				if err := session.sessionLock.Close(); err != nil {
					t.Fatal(err)
				}
				session.sessionLock = nil
			},
			forceCreatedLock: true,
		},
		{
			name: "unexpected-entry",
			prepare: func(t *testing.T, session *Session, _ string) {
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
			prepare: func(t *testing.T, _ *Session, sessionPath string) {
				if err := os.WriteFile(filepath.Join(sessionPath, resumestate.HeaderRecordName), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stage-is-not-directory",
			prepare: func(t *testing.T, session *Session, sessionPath string) {
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
			prepare: func(t *testing.T, session *Session, sessionPath string) {
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

			directory := outputcap.Directory(session.sessionDir)
			if test.headerOpenFault || test.forceCreatedLock {
				directory = &terminalFaultDirectory{
					Directory:        session.sessionDir,
					openFileName:     resumestate.HeaderRecordName,
					forceOpenFileErr: test.headerOpenFault,
					forceCreatedLock: test.forceCreatedLock,
				}
			}
			layout, err := outputnamespace.InspectTerminalLayout(directory, session.state.Header(), nil)
			if layout != nil {
				_ = layout.Close()
			}
			if err == nil {
				t.Fatal("ambiguous or owned terminal cut unexpectedly accepted")
			}
		})
	}
}

func TestOutputV3TerminalAuthorityVerificationRejectsStaleBindings(t *testing.T) {
	t.Parallel()
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
		intent    outputcap.Directory
		session   outputcap.Directory
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
			err := outputnamespace.VerifyTerminalAuthority(
				session.control, test.intent, test.session, test.header,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("terminal authority verification error = %v, want error=%t", err, test.wantError)
			}
		})
	}
}

func TestOutputV3TerminalInspectionAndShellRemovalFailBeforeMutation(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)

	directory := &terminalFaultDirectory{
		Directory: session.sessionDir,
		namesErr:  errTerminalRecoveryInjected,
	}
	if layout, err := outputnamespace.InspectTerminalLayout(directory, session.state.Header(), nil); layout != nil || !errors.Is(err, errTerminalRecoveryInjected) {
		t.Fatalf("terminal layout enumeration = (%v, %v), want nil and injected failure", layout, err)
	}
	intentName := resumestate.ResumeNamespaceName(session.state.Header().ResumeIntent())
	sessionName := resumestate.SessionDirectoryName(session.SessionID())
	if err := outputnamespace.RemoveEmptySessionShell(
		session.control.Sessions(), session.intentDir, session.sessionDir, intentName, sessionName,
	); err == nil {
		t.Fatal("non-empty session shell was removed")
	}
	if kind, err := session.intentDir.ObserveEntry(sessionName); err != nil || kind != outputcap.EntryDirectory {
		t.Fatalf("session shell after rejected removal = (%v, %v), want directory", kind, err)
	}
}

func TestOutputV3TerminalSessionRecoveryPropagatesPreflightFailuresBeforeRetirement(t *testing.T) {
	t.Parallel()
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
				unexpected, err := fixture.stages.CreateFile("unexpected", true, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(unexpected.Sync(), fixture.stages.Sync(), unexpected.Close()); err != nil {
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
			if err := fixture.layout.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.layout = &outputnamespace.TerminalLayout{}
			intentDirectory := outputcap.Directory(fixture.intentDirectory)
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

func TestOutputV3TerminalSessionRecoveryJoinsLayoutCloseFailure(t *testing.T) {
	t.Parallel()
	fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionCompleting)
	defer fixture.close(t)
	fixture.sessionDirectory.childNamesErrAt = map[string]int{
		resumestate.FilesDirectoryName: 2,
	}
	fixture.sessionDirectory.childCloseErr = map[string]error{
		resumestate.StagesDirectoryName: errTerminalRecoveryLayoutCloseInjected,
	}
	if err := fixture.layout.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.layout = &outputnamespace.TerminalLayout{}

	attention, err := fixture.session.owner.recoverTerminalSession(
		fixture.session.platform,
		fixture.control,
		fixture.intentDirectory,
		fixture.sessionDirectory,
		fixture.session.state,
		outputSelectionAdmission{
			selection: fixture.session.selection,
			files:     fixture.session.selectedFiles,
			dirs:      fixture.session.selectedDirs,
		},
	)
	if attention || !errors.Is(err, errTerminalRecoveryInjected) ||
		!errors.Is(err, errTerminalRecoveryLayoutCloseInjected) {
		t.Fatalf("terminal recovery with layout-close failure = (attention=%t, %v), want both failures",
			attention, err)
	}
	assertTerminalRecoveryCut(t, fixture, 0, true, true)
}

func TestOutputV3DiscardHeaderStopsAfterAdoptedFixedReopenCloseFailure(t *testing.T) {
	t.Parallel()
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
		fixture.session.store, fixture.control.Control(), fixture.sessionDirectory,
		reference, true, func() error { return nil },
	)
	if !errors.Is(err, errTerminalRecoveryInjected) || removable || corrupt || namespace.Header().StateGeneration() != 0 {
		t.Fatalf("discard header close cut = (removable=%t, corrupt=%t, lifecycle=%v, err=%v)",
			removable, corrupt, namespace.Header().Lifecycle(), err)
	}
	encoded, readErr := outputnamespace.ReadRecord(
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
		if observeErr != nil || kind != outputcap.EntryDirectory {
			t.Fatalf("discard close cut child %q = (kind=%v, err=%v)", name, kind, observeErr)
		}
	}
}

type terminalRecoveryFaultFixture struct {
	session           *Session
	control           *outputnamespace.ControlNamespace
	sessionsDirectory *terminalFaultDirectory
	intentDirectory   *terminalFaultDirectory
	sessionDirectory  *terminalFaultDirectory
	layout            *outputnamespace.TerminalLayout
	stages            *terminalFaultDirectory
	anchors           *terminalFaultDirectory
	files             *terminalFaultDirectory
	lock              *terminalFaultLock
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
	releaseErr := errors.Join(
		session.stagesDir.Close(), session.anchorsDir.Close(), session.filesDir.Close(), session.sessionLock.Close(),
	)
	session.stagesDir, session.anchorsDir, session.filesDir, session.sessionLock = nil, nil, nil, nil
	session.mu.Unlock()
	if releaseErr != nil {
		v3RecoveryCloseSession(t, session)
		t.Fatal(releaseErr)
	}

	header := session.state.Header()
	rootDirectory := &terminalFaultDirectory{Directory: session.platform.Root()}
	control, err := session.owner.namespaceController().OpenInstalledControl(rootDirectory, session.platform)
	if err != nil {
		v3RecoveryCloseSession(t, session)
		t.Fatal(err)
	}
	sessionsDirectory, ok := control.Sessions().(*terminalFaultDirectory)
	if !ok {
		_ = control.Close()
		v3RecoveryCloseSession(t, session)
		t.Fatal("decorated control did not retain the sessions fault boundary")
	}
	intent, err := outputnamespace.OpenCanonicalIntent(sessionsDirectory, header.ResumeIntent())
	if err != nil {
		_ = control.Close()
		v3RecoveryCloseSession(t, session)
		t.Fatal(err)
	}
	intentDirectory, ok := intent.(*terminalFaultDirectory)
	if !ok {
		_ = errors.Join(intent.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatal("decorated control did not retain the intent fault boundary")
	}
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	openedSession, err := intentDirectory.OpenDirectory(sessionName, true)
	if err != nil {
		_ = errors.Join(intentDirectory.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatal(err)
	}
	sessionDirectory, ok := openedSession.(*terminalFaultDirectory)
	if !ok {
		_ = errors.Join(openedSession.Close(), intentDirectory.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatal("decorated control did not retain the session fault boundary")
	}
	rawLock, created, err := sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
	if err != nil || created {
		_ = errors.Join(sessionDirectory.Close(), intentDirectory.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatalf("reacquire terminal fixture lock = (created=%t, %v)", created, err)
	}
	lock := &terminalFaultLock{Lock: rawLock}
	layout, err := outputnamespace.InspectTerminalLayout(
		sessionDirectory, header, func() (outputcap.Lock, error) { return lock, nil },
	)
	if err != nil {
		_ = errors.Join(lock.Close(), sessionDirectory.Close(), intentDirectory.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatal(err)
	}
	stages, stagesOK := layout.Stages().(*terminalFaultDirectory)
	anchors, anchorsOK := layout.Anchors().(*terminalFaultDirectory)
	files, filesOK := layout.Files().(*terminalFaultDirectory)
	if !stagesOK || !anchorsOK || !filesOK {
		_ = errors.Join(layout.Close(), sessionDirectory.Close(), intentDirectory.Close(), control.Close())
		v3RecoveryCloseSession(t, session)
		t.Fatal("terminal layout lost a decorated directory fault boundary")
	}
	return &terminalRecoveryFaultFixture{
		session: session, control: control,
		sessionsDirectory: sessionsDirectory,
		intentDirectory:   intentDirectory,
		sessionDirectory:  sessionDirectory,
		layout:            layout, stages: stages, anchors: anchors, files: files, lock: lock, header: header,
		intentName:  resumestate.ResumeNamespaceName(header.ResumeIntent()),
		sessionName: sessionName,
	}
}

func (fixture *terminalRecoveryFaultFixture) close(t *testing.T) {
	t.Helper()
	if err := errors.Join(
		fixture.layout.Close(), fixture.sessionDirectory.Close(), fixture.intentDirectory.Close(),
		fixture.control.Close(), fixture.session.closeHandles(),
	); err != nil {
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
	intentKind, err := fixture.control.Sessions().ObserveEntry(fixture.intentName)
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
	sessionKind, err := fixture.session.intentDir.ObserveEntry(fixture.sessionName)
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
	for index, name := range []string{
		resumestate.StagesDirectoryName,
		resumestate.AnchorsDirectoryName,
		resumestate.FilesDirectoryName,
		resumestate.SessionLockName,
		resumestate.HeaderRecordName,
	} {
		kind, err := fixture.session.sessionDir.ObserveEntry(name)
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

var (
	errTerminalRecoveryInjected            = errors.New("injected terminal recovery failure")
	errTerminalRecoveryLayoutCloseInjected = errors.New("injected terminal recovery layout close failure")
)

type terminalFaultDirectory struct {
	outputcap.Directory
	namesErr            error
	namesErrAt          int
	namesCalls          int
	childNamesErrAt     map[string]int
	childCloseErr       map[string]error
	openDirectoryErrAt  int
	openDirectoryCalls  int
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
) (outputcap.File, error) {
	file, err := directory.Directory.CreateFile(name, private, size)
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
	return directory.Directory.Names(limit)
}

func (directory *terminalFaultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	directory.openDirectoryCalls++
	if directory.openDirectoryErrAt > 0 && directory.openDirectoryCalls == directory.openDirectoryErrAt {
		return nil, errStateTerminalInjected
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &terminalFaultDirectory{
		Directory:  opened,
		namesErrAt: directory.childNamesErrAt[name],
		closeErr:   directory.childCloseErr[name],
	}, nil
}

func (directory *terminalFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
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
	file, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	closeErr := error(nil)
	if matched && directory.openFileCalls == directory.openFileCloseErrAt {
		closeErr = errTerminalRecoveryInjected
	}
	if matched && (directory.readFileErr != nil || closeErr != nil) {
		return &terminalFaultFile{
			File: file, readErr: directory.readFileErr, closeErr: closeErr,
		}, nil
	}
	return file, nil
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

func (directory *terminalFaultDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	if directory.replaceErr != nil {
		return directory.replaceErr
	}
	if wrapped, ok := source.(*terminalFaultFile); ok {
		source = wrapped.File
	}
	return directory.Directory.ReplacePrivateFile(source, name)
}

func (directory *terminalFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapTerminalFaultDirectory(other))
}

func (directory *terminalFaultDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	if name == directory.removeDirectoryName {
		return errTerminalRecoveryInjected
	}
	return directory.Directory.RemoveDirectory(name, unwrapTerminalFaultDirectory(expected))
}

func (directory *terminalFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if name == directory.removeFileName {
		return errTerminalRecoveryInjected
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *terminalFaultDirectory) Sync() error {
	directory.syncCalls++
	if directory.syncErrAt > 0 && directory.syncCalls == directory.syncErrAt {
		return errTerminalRecoveryInjected
	}
	return directory.Directory.Sync()
}

func (directory *terminalFaultDirectory) Close() error {
	if directory.closed {
		return nil
	}
	directory.closed = true
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

func unwrapTerminalFaultDirectory(directory outputcap.Directory) outputcap.Directory {
	for {
		switch wrapped := directory.(type) {
		case *terminalFaultDirectory:
			directory = wrapped.Directory
		case *stateTerminalAuthorityDirectory:
			directory = wrapped.Directory
		default:
			return directory
		}
	}
}

type terminalFaultFile struct {
	outputcap.File
	readErr  error
	closeErr error
}

func (file *terminalFaultFile) ReadAt(data []byte, offset int64) (int, error) {
	if file.readErr != nil {
		return 0, file.readErr
	}
	return file.File.ReadAt(data, offset)
}

func (file *terminalFaultFile) Close() error {
	return errors.Join(file.File.Close(), file.closeErr)
}

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
	if lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(lock.Lock.Close(), lock.closeErr)
}
