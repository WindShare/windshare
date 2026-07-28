package outputnamespace

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestTerminalRecoveryFailureCutsRemainRestartable(t *testing.T) {
	for _, test := range []struct {
		name               string
		lifecycle          resumestate.SessionLifecycle
		discard            bool
		configure          func(*testing.T, *terminalNamespaceFixture)
		wantRemovedEntries int
		wantSession        bool
		wantIntent         bool
		wantInjected       bool
	}{
		{
			name: "discard-child-enumeration", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.layout.stages.(*terminalFaultDirectory).namesErr = errTerminalInjected
			},
			wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "completing-child-not-empty", lifecycle: resumestate.SessionCompleting,
			configure: func(t *testing.T, fixture *terminalNamespaceFixture) {
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
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.removeDirectoryName = resumestate.StagesDirectoryName
			},
			wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-stage", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 1, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "close-stage", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.layout.stages.(*terminalFaultDirectory).closeErr = errTerminalInjected
			},
			wantRemovedEntries: 1, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "missing-lock-file-authority", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.layout.lock = &terminalFaultLock{Lock: fixture.layout.lock, nilFile: true}
			},
			wantRemovedEntries: 3, wantSession: true, wantIntent: true,
		},
		{
			name: "remove-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.removeFileName = resumestate.SessionLockName
			},
			wantRemovedEntries: 3, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.syncErrAt = 4
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "close-lock", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.layout.lock = &terminalFaultLock{Lock: fixture.layout.lock, closeErr: errTerminalInjected}
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-header", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.removeFileName = resumestate.HeaderRecordName
			},
			wantRemovedEntries: 4, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-header", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionDirectory.syncErrAt = 5
			},
			wantRemovedEntries: 5, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-session-shell", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.intentDirectory.removeDirectoryName = fixture.sessionName
			},
			wantRemovedEntries: 5, wantSession: true, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-intent", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.intentDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 5, wantIntent: true, wantInjected: true,
		},
		{
			name: "remove-intent", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionsDirectory.removeDirectoryName = fixture.intentName
			},
			wantRemovedEntries: 5, wantIntent: true, wantInjected: true,
		},
		{
			name: "sync-sessions", lifecycle: resumestate.SessionDiscarding, discard: true,
			configure: func(_ *testing.T, fixture *terminalNamespaceFixture) {
				fixture.sessionsDirectory.syncErrAt = 1
			},
			wantRemovedEntries: 5, wantInjected: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTerminalNamespaceFixture(t, test.lifecycle, true)
			defer fixture.close(t)
			test.configure(t, fixture)
			err := RecoverTerminalNamespace(
				fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
				fixture.header, fixture.layout, test.discard,
			)
			if err == nil || test.wantInjected && !errors.Is(err, errTerminalInjected) {
				t.Fatalf("terminal cut error = %v, want injected=%t", err, test.wantInjected)
			}
			assertTerminalCut(t, fixture, test.wantRemovedEntries, test.wantSession, test.wantIntent)
		})
	}
}

func TestTerminalRecoveryCompletesDeterministicCuts(t *testing.T) {
	for _, test := range []struct {
		name      string
		lifecycle resumestate.SessionLifecycle
		discard   bool
		populate  bool
	}{
		{name: "complete", lifecycle: resumestate.SessionCompleting},
		{name: "discard-nested", lifecycle: resumestate.SessionDiscarding, discard: true, populate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTerminalNamespaceFixture(t, test.lifecycle, true)
			defer fixture.close(t)
			if test.populate {
				nested, err := fixture.layout.stages.CreateDirectory("nested", true)
				if err != nil {
					t.Fatal(err)
				}
				file, err := nested.CreateFile("part", true, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), file.Close(), nested.Sync(), nested.Close()); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverTerminalNamespace(
				fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
				fixture.header, fixture.layout, test.discard,
			); err != nil {
				t.Fatal(err)
			}
			assertTerminalCut(t, fixture, len(terminalRemovalOrder), false, false)
		})
	}
}

func TestTerminalLayoutAndAuthorityRejectAmbiguousState(t *testing.T) {
	t.Run("inspection", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			prepare func(*testing.T, *terminalNamespaceFixture) outputcap.Directory
		}{
			{
				name: "live-lock",
				prepare: func(t *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					lock, _, err := fixture.sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
					if err != nil {
						t.Fatal(err)
					}
					fixture.heldLock = lock
					return fixture.sessionDirectory
				},
			},
			{
				name: "header-reopen",
				prepare: func(_ *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					return &terminalFaultDirectory{
						Directory: fixture.sessionDirectory, openFileName: resumestate.HeaderRecordName,
						forceOpenFileErr: true,
					}
				},
			},
			{
				name: "recreated-lock",
				prepare: func(_ *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					return &terminalFaultDirectory{Directory: fixture.sessionDirectory, forceCreatedLock: true}
				},
			},
			{
				name: "unexpected-entry",
				prepare: func(t *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					file, err := fixture.sessionDirectory.CreateFile("unexpected", true, 0)
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					return fixture.sessionDirectory
				},
			},
			{
				name: "corrupt-header",
				prepare: func(t *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					file, err := fixture.sessionDirectory.OpenFile(resumestate.HeaderRecordName, true, true)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteAt([]byte("corrupt"), 0)
					if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
						t.Fatal(err)
					}
					return fixture.sessionDirectory
				},
			},
			{
				name: "stage-is-file",
				prepare: func(t *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					stage, err := fixture.sessionDirectory.OpenDirectory(resumestate.StagesDirectoryName, true)
					if err != nil {
						t.Fatal(err)
					}
					if err := errors.Join(
						fixture.sessionDirectory.RemoveDirectory(resumestate.StagesDirectoryName, stage), stage.Close(),
					); err != nil {
						t.Fatal(err)
					}
					file, err := fixture.sessionDirectory.CreateFile(resumestate.StagesDirectoryName, true, 0)
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
					return fixture.sessionDirectory
				},
			},
			{
				name: "nonempty-lock",
				prepare: func(t *testing.T, fixture *terminalNamespaceFixture) outputcap.Directory {
					file, err := fixture.sessionDirectory.OpenFile(resumestate.SessionLockName, true, true)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteAt([]byte{1}, 0)
					if err := errors.Join(writeErr, file.Close()); err != nil {
						t.Fatal(err)
					}
					return fixture.sessionDirectory
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newTerminalNamespaceFixture(t, resumestate.SessionCompleting, false)
				defer fixture.close(t)
				layout, err := InspectTerminalLayout(test.prepare(t, fixture), fixture.header, nil)
				if layout != nil {
					_ = layout.Close()
				}
				if err == nil {
					t.Fatal("ambiguous terminal layout was accepted")
				}
			})
		}
	})

	t.Run("authority", func(t *testing.T) {
		fixture := newTerminalNamespaceFixture(t, resumestate.SessionCompleting, true)
		defer fixture.close(t)
		next, err := resumestate.BindSessionNamespaceAuthority(
			fixture.control.Control(), fixture.header, fixture.intentName, fixture.sessionName,
		)
		if err != nil {
			t.Fatal(err)
		}
		stale, err := next.WithLifecycle(resumestate.SessionPausedNeedsAttention)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name    string
			intent  outputcap.Directory
			session outputcap.Directory
			header  resumestate.Header
			valid   bool
		}{
			{name: "current", intent: fixture.intentDirectory, session: fixture.sessionDirectory, header: fixture.header, valid: true},
			{name: "intent-rebound", intent: fixture.sessionDirectory, session: fixture.sessionDirectory, header: fixture.header},
			{name: "session-rebound", intent: fixture.intentDirectory, session: fixture.layout.stages, header: fixture.header},
			{name: "stale-header", intent: fixture.intentDirectory, session: fixture.sessionDirectory, header: stale.Header()},
		} {
			t.Run(test.name, func(t *testing.T) {
				err := VerifyTerminalAuthority(fixture.control, test.intent, test.session, test.header)
				if (err == nil) != test.valid {
					t.Fatalf("authority error = %v, want valid=%t", err, test.valid)
				}
			})
		}
	})
}

func TestTerminalRecoveryRevalidatesEveryRemovalBoundary(t *testing.T) {
	for _, failAt := range []int{1, 2, 7, 8, 9, 10} {
		fixture := newTerminalNamespaceFixture(t, resumestate.SessionDiscarding, true)
		counter := &terminalAuthorityDirectory{Directory: fixture.control.Sessions(), failAt: failAt}
		fixture.control.sessions = counter
		err := RecoverTerminalNamespace(
			fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
			fixture.header, fixture.layout, true,
		)
		if !errors.Is(err, errTerminalInjected) || counter.openCalls != failAt {
			t.Errorf("authority cut %d = (%v, calls=%d)", failAt, err, counter.openCalls)
		}
		fixture.close(t)
	}
}

func TestTerminalHeaderAndShellRemovalRequireExactAuthority(t *testing.T) {
	for _, test := range []struct {
		name         string
		zero         bool
		stale        bool
		openErr      error
		readErr      error
		wantInjected bool
	}{
		{name: "invalid-header", zero: true},
		{name: "stale-header", stale: true},
		{name: "open-failure", openErr: errTerminalInjected, wantInjected: true},
		{name: "read-failure", readErr: errTerminalInjected, wantInjected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTerminalNamespaceFixture(t, resumestate.SessionActive, false)
			defer fixture.close(t)
			expected := fixture.header
			if test.zero {
				expected = resumestate.Header{}
			}
			if test.stale {
				namespace, err := resumestate.BindSessionNamespaceAuthority(
					fixture.control.Control(), fixture.header, fixture.intentName, fixture.sessionName,
				)
				if err != nil {
					t.Fatal(err)
				}
				next, err := namespace.WithLifecycle(resumestate.SessionPausing)
				if err != nil {
					t.Fatal(err)
				}
				expected = next.Header()
			}
			directory := &terminalFaultDirectory{
				Directory: fixture.sessionDirectory, openFileName: resumestate.HeaderRecordName,
				openFileErr: test.openErr, readFileErr: test.readErr,
			}
			err := removeTerminalHeader(directory, expected)
			if err == nil || test.wantInjected && !errors.Is(err, errTerminalInjected) {
				t.Fatalf("header removal error = %v, want injected=%t", err, test.wantInjected)
			}
			kind, observeErr := ObserveExactEntry(fixture.sessionDirectory, resumestate.HeaderRecordName)
			if observeErr != nil || kind != outputcap.EntryRegularFile {
				t.Fatalf("header after rejected removal = (%v, %v)", kind, observeErr)
			}
		})
	}

	fixture := newTerminalNamespaceFixture(t, resumestate.SessionCompleting, false)
	defer fixture.close(t)
	directory := &terminalFaultDirectory{Directory: fixture.sessionDirectory, namesErr: errTerminalInjected}
	if layout, err := InspectTerminalLayout(directory, fixture.header, nil); layout != nil || !errors.Is(err, errTerminalInjected) {
		t.Fatalf("layout enumeration = (%v, %v)", layout, err)
	}
	if err := RemoveEmptySessionShell(
		fixture.control.Sessions(), fixture.intentDirectory, fixture.sessionDirectory,
		fixture.intentName, fixture.sessionName,
	); err == nil {
		t.Fatal("non-empty session shell was removed")
	}
	if err := (*TerminalLayout)(nil).Close(); err != nil {
		t.Fatalf("close nil terminal layout: %v", err)
	}
}

type outputV3CandidateFaultDirectory struct {
	outputcap.Directory
	namesErr               error
	classifyErr            error
	forceInexact           bool
	createErr              error
	createdSyncErr         error
	syncErr                error
	openFileName           string
	openFileErr            error
	fileSizeErr            error
	fileCloseErr           error
	openDirectoryName      string
	openDirectoryErr       error
	childNamesErr          map[string]error
	childCloseErr          map[string]error
	removeDirectoryErr     map[string]error
	installErr             error
	forceInstalledMismatch bool
	forceSameMismatch      bool
	closeErr               error
}

func (directory *outputV3CandidateFaultDirectory) Names(limit int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	return directory.Directory.Names(limit)
}

func (directory *outputV3CandidateFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.classifyErr != nil {
		return outputcap.EntryAbsent, false, directory.classifyErr
	}
	kind, exact, err := directory.Directory.ClassifyExactEntry(name)
	if directory.forceInexact {
		exact = false
	}
	return kind, exact, err
}

func (directory *outputV3CandidateFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.createErr != nil {
		return nil, directory.createErr
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3CandidateFaultDirectory{
		Directory: created,
		syncErr:   directory.createdSyncErr,
	}, nil
}

func (directory *outputV3CandidateFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if name == directory.openFileName && directory.openFileErr != nil {
		return nil, directory.openFileErr
	}
	file, err := directory.Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	if name == directory.openFileName && (directory.fileSizeErr != nil || directory.fileCloseErr != nil) {
		return &outputV3CandidateFaultFile{
			File: file, sizeErr: directory.fileSizeErr, closeErr: directory.fileCloseErr,
		}, nil
	}
	return file, nil
}

func (directory *outputV3CandidateFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if name == directory.openDirectoryName && directory.openDirectoryErr != nil {
		return nil, directory.openDirectoryErr
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3CandidateFaultDirectory{
		Directory: opened,
		namesErr:  directory.childNamesErr[name],
		closeErr:  directory.childCloseErr[name],
	}, nil
}

func (directory *outputV3CandidateFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if directory.forceSameMismatch {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3CandidateFaultDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *outputV3CandidateFaultDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if directory.installErr != nil {
		return nil, directory.installErr
	}
	if wrapped, ok := candidate.(*outputV3CandidateFaultDirectory); ok {
		candidate = wrapped.Directory
	}
	installed, err := directory.Directory.InstallDirectoryNoReplace(candidate, name)
	if err != nil {
		return nil, err
	}
	return &outputV3CandidateFaultDirectory{
		Directory:         installed,
		forceSameMismatch: directory.forceInstalledMismatch,
	}, nil
}

func (directory *outputV3CandidateFaultDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	if err := directory.removeDirectoryErr[name]; err != nil {
		return err
	}
	if wrapped, ok := expected.(*outputV3CandidateFaultDirectory); ok {
		expected = wrapped.Directory
	}
	return directory.Directory.RemoveDirectory(name, expected)
}

func (directory *outputV3CandidateFaultDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.Directory.Sync()
}

func (directory *outputV3CandidateFaultDirectory) Close() error {
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

type outputV3CandidateFaultFile struct {
	outputcap.File
	sizeErr  error
	closeErr error
	closed   bool
}

func (file *outputV3CandidateFaultFile) Size() (uint64, error) {
	if file.sizeErr != nil {
		return 0, file.sizeErr
	}
	return file.File.Size()
}

func (file *outputV3CandidateFaultFile) Close() error {
	if file.closed {
		return nil
	}
	file.closed = true
	return errors.Join(file.File.Close(), file.closeErr)
}
