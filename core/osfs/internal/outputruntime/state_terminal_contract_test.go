package outputruntime

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3StateStoreRejectsUnprovableCreationAndReplacementCuts(t *testing.T) {
	t.Parallel()
	current, next, _, currentEncoded := stateStoreHeaderImages(t)

	t.Run("invalid-create-image", func(t *testing.T) {
		if _, err := (outputnamespace.Store{}).CreateRecord(nil, "state", nil, 1); err == nil {
			t.Fatal("empty state image reached the filesystem")
		}
	})

	t.Run("replacement-nonce-source", func(t *testing.T) {
		outcome, err := outputnamespace.NewStore(outputnamespace.StoreConfig{Random: bytes.NewReader(nil)}).ReplaceRecord(
			nil, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputnamespace.ReplaceUnchanged || err == nil {
			t.Fatalf("nonce failure = (%v, %v), want unchanged error", outcome, err)
		}
	})

	t.Run("replacement-collision-budget", func(t *testing.T) {
		faults := &stateStoreFaultDirectory{fault: stateStoreFaultCreateCollision}
		outcome, err := outputnamespace.NewStore(outputnamespace.StoreConfig{
			Random: bytes.NewReader(bytes.Repeat([]byte{0x31}, 2048)),
		}).ReplaceRecord(
			faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputnamespace.ReplaceUnchanged || err == nil {
			t.Fatalf("collision exhaustion = (%v, %v), want unchanged error", outcome, err)
		}
	})

	t.Run("replacement-short-write", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, currentEncoded)
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreFaultDirectory{
			Directory: directory,
			fault:     stateStoreFaultShortWrite,
			target:    resumestate.HeaderRecordName,
		}
		outcome, err := outputnamespace.NewStore(outputnamespace.StoreConfig{
			Random: bytes.NewReader(bytes.Repeat([]byte{0x32}, 128)),
		}).ReplaceRecord(
			faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputnamespace.ReplaceUnchanged || err == nil {
			t.Fatalf("short replacement write = (%v, %v), want unchanged error", outcome, err)
		}
	})

	for _, test := range []struct {
		name             string
		sameFileMismatch bool
		targetOpenErr    bool
	}{
		{name: "linked-target-identity-mismatch", sameFileMismatch: true},
		{name: "installed-target-cannot-be-reopened", targetOpenErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, directory := stateStoreEmptyDirectoryFixture(t)
			defer closeStateStoreFixture(t, platform, directory)
			faults := &stateTerminalCreateDirectory{
				Directory:        directory,
				target:           resumestate.HeaderRecordName,
				sameFileMismatch: test.sameFileMismatch,
				targetOpenErr:    test.targetOpenErr,
			}
			_, err := outputnamespace.NewStore(outputnamespace.StoreConfig{
				Random: bytes.NewReader(bytes.Repeat([]byte{0x33}, 128)),
			}).CreateRecord(faults, resumestate.HeaderRecordName, []byte("image"), 5)
			if err == nil {
				t.Fatal("unproved installed state was accepted")
			}
		})
	}
}

func TestOutputV3StateRecoveryRejectsAmbiguousObservationBoundaries(t *testing.T) {
	t.Parallel()
	t.Run("initial-target-observation", func(t *testing.T) {
		directory := &stateTerminalClassifyDirectory{classifyErr: errStateTerminalInjected}
		if _, err := (outputnamespace.Store{}).EnsureInitialRecord(directory, "record", []byte{1}, 1); !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("initial observation error = %v", err)
		}
	})

	t.Run("header-reconcile-requires-authority", func(t *testing.T) {
		if err := outputnamespace.ReconcileHeaderRecordTemporaries(nil, resumestate.SessionNamespaceAuthority{}, nil); err == nil {
			t.Fatal("header reconciliation accepted absent authority verifier")
		}
		if err := outputnamespace.ReconcileHeaderRecordTemporaries(
			nil, resumestate.SessionNamespaceAuthority{}, func() error { return nil },
		); err == nil {
			t.Fatal("header reconciliation accepted an invalid namespace authority")
		}
	})

	root := v3RecoveryRoot(t)
	session := v3RecoveryOpen(
		t, v3RecoveryAuthority(t, root, nil), root, v3RecoverySelection(t, false, 0),
	).Session
	defer v3RecoveryCloseSession(t, session)
	namespace := session.state.NamespaceAuthority()

	t.Run("installed-header-authority", func(t *testing.T) {
		err := outputnamespace.ReconcileHeaderRecordTemporaries(
			session.sessionDir, namespace, func() error { return errStateTerminalInjected },
		)
		if !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("installed authority error = %v", err)
		}
	})

	t.Run("temporary-enumeration", func(t *testing.T) {
		directory := &stateStoreReconcileFaultDirectory{
			Directory: session.sessionDir,
			namesErr:  errStateTerminalInjected,
		}
		err := outputnamespace.ReconcileHeaderRecordTemporaries(directory, namespace, func() error { return nil })
		if !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("temporary enumeration error = %v", err)
		}
	})

	nonce, err := resumestate.GenerateUpdateNonce(bytes.NewReader(bytes.Repeat([]byte{0x34}, resumestate.UpdateNonceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	temporaryName, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		classifyErr error
		openErr     error
	}{
		{name: "temporary-observation", classifyErr: errStateTerminalInjected},
		{name: "temporary-open", openErr: errStateTerminalInjected},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &stateStoreReconcileFaultDirectory{
				Directory:        session.sessionDir,
				namesOverride:    []string{temporaryName},
				classifyOverride: true,
				classifyName:     temporaryName,
				classifyKind:     outputcap.EntryRegularFile,
				classifyErr:      test.classifyErr,
				openName:         temporaryName,
				openErr:          test.openErr,
			}
			err := outputnamespace.ReconcileHeaderRecordTemporaries(directory, namespace, func() error { return nil })
			if !errors.Is(err, errStateTerminalInjected) {
				t.Fatalf("temporary inspection error = %v", err)
			}
		})
	}

	if _, err := outputnamespace.ReadRecord(
		&stateTerminalClassifyDirectory{classifyErr: errStateTerminalInjected}, "record", 1,
	); !errors.Is(err, errStateTerminalInjected) {
		t.Fatalf("state record observation error = %v", err)
	}
	if _, err := outputnamespace.ReadRecord(
		&stateTerminalClassifyDirectory{kind: outputcap.EntryDirectory, exact: true}, "record", 1,
	); err == nil {
		t.Fatal("directory was accepted as a state record")
	}
	if _, err := outputnamespace.ObserveExactEntry(
		&stateTerminalClassifyDirectory{kind: outputcap.EntryRegularFile}, "record",
	); err == nil {
		t.Fatal("non-exact state entry was accepted")
	}
}

func TestOutputV3TerminalRecoveryRevalidatesAuthorityAtEveryRemovalBoundary(t *testing.T) {
	t.Parallel()
	for _, failAt := range []int{1, 2, 7, 8, 9, 10} {
		t.Run(fmt.Sprintf("authority-check-%d", failAt), func(t *testing.T) {
			fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
			defer fixture.close(t)
			counter := fixture.sessionsDirectory
			counter.openDirectoryCalls = 0
			counter.openDirectoryErrAt = failAt
			err := outputnamespace.RecoverTerminalNamespace(
				fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
				fixture.header, fixture.layout, true,
			)
			if !errors.Is(err, errStateTerminalInjected) || counter.openDirectoryCalls != failAt {
				t.Fatalf("authority cut %d = (%v, calls=%d)", failAt, err, counter.openDirectoryCalls)
			}
		})
	}
}

func TestOutputV3TerminalShellAndLockObservationFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("session-binding-before-shell-removal", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		counter := &stateTerminalAuthorityDirectory{
			Directory: fixture.intentDirectory,
			failAt:    9,
		}
		err := outputnamespace.RecoverTerminalNamespace(
			fixture.control, counter, fixture.sessionDirectory,
			fixture.header, fixture.layout, true,
		)
		if !errors.Is(err, errStateTerminalInjected) || counter.openCalls != 9 {
			t.Fatalf("session shell binding cut = (%v, calls=%d)", err, counter.openCalls)
		}
	})

	t.Run("intent-enumeration-after-session-removal", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		fixture.intentDirectory.namesErr = errStateTerminalInjected
		err := outputnamespace.RecoverTerminalNamespace(
			fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
			fixture.header, fixture.layout, true,
		)
		if !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("intent enumeration cut = %v", err)
		}
	})

	t.Run("already-removed-session-shell", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		intent := &stateTerminalAuthorityDirectory{
			Directory:     fixture.intentDirectory,
			removeMissing: true,
		}
		if err := outputnamespace.RecoverTerminalNamespace(
			fixture.control, intent, fixture.sessionDirectory,
			fixture.header, fixture.layout, true,
		); err != nil {
			t.Fatalf("already-removed session shell: %v", err)
		}
	})

	t.Run("enumerated-lock-has-no-file-authority", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		session := v3RecoveryOpen(
			t, v3RecoveryAuthority(t, root, nil), root, v3RecoverySelection(t, false, 0),
		).Session
		defer v3RecoveryCloseSession(t, session)
		if err := session.installLifecycle(resumestate.SessionCompleting); err != nil {
			t.Fatal(err)
		}
		if err := session.sessionLock.Close(); err != nil {
			t.Fatal(err)
		}
		session.sessionLock = nil
		layout, err := outputnamespace.InspectTerminalLayout(
			&stateTerminalNilFileLockDirectory{Directory: session.sessionDir},
			session.state.Header(),
			nil,
		)
		if layout != nil {
			_ = layout.Close()
		}
		if err == nil {
			t.Fatal("terminal layout accepted a lock without file authority")
		}
	})

	t.Run("header-reread", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		sessionDirectory := &terminalFaultDirectory{
			Directory:        unwrapStateTerminalDirectory(fixture.sessionDirectory),
			openFileName:     resumestate.HeaderRecordName,
			forceOpenFileErr: true,
		}
		err := outputnamespace.VerifyTerminalAuthority(
			fixture.control, fixture.intentDirectory, sessionDirectory, fixture.header,
		)
		if !errors.Is(err, errTerminalRecoveryInjected) {
			t.Fatalf("terminal header reread error = %v", err)
		}
	})

	if err := (*outputnamespace.TerminalLayout)(nil).Close(); err != nil {
		t.Fatalf("close nil terminal layout: %v", err)
	}
}

var errStateTerminalInjected = errors.New("injected state/terminal authority cut")

type stateTerminalClassifyDirectory struct {
	outputcap.Directory
	kind        outputcap.EntryKind
	exact       bool
	classifyErr error
}

func (directory *stateTerminalClassifyDirectory) ClassifyExactEntry(
	string,
) (outputcap.EntryKind, bool, error) {
	return directory.kind, directory.exact, directory.classifyErr
}

type stateTerminalCreateDirectory struct {
	outputcap.Directory
	target           string
	sameFileMismatch bool
	targetOpenErr    bool
}

func (directory *stateTerminalCreateDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if name == directory.target && directory.targetOpenErr {
		return nil, errStateTerminalInjected
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *stateTerminalCreateDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	linked, err := directory.Directory.LinkFileNoReplace(source, name)
	if err != nil || !directory.sameFileMismatch {
		return linked, err
	}
	return &stateTerminalDifferentFile{File: linked}, nil
}

type stateTerminalDifferentFile struct{ outputcap.File }

func (*stateTerminalDifferentFile) SameFile(outputcap.File) (bool, error) { return false, nil }

type stateTerminalAuthorityDirectory struct {
	outputcap.Directory
	openCalls     int
	failAt        int
	removeMissing bool
}

func (directory *stateTerminalAuthorityDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	directory.openCalls++
	if directory.openCalls == directory.failAt {
		return nil, errStateTerminalInjected
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &stateTerminalAuthorityDirectory{Directory: opened}, nil
}

func (directory *stateTerminalAuthorityDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	if directory.removeMissing {
		return fs.ErrNotExist
	}
	return directory.Directory.RemoveDirectory(name, unwrapStateTerminalDirectory(expected))
}

func (directory *stateTerminalAuthorityDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return unwrapStateTerminalDirectory(directory.Directory).SameDirectory(
		unwrapStateTerminalDirectory(other),
	)
}

func unwrapStateTerminalDirectory(directory outputcap.Directory) outputcap.Directory {
	for {
		switch wrapped := directory.(type) {
		case *stateTerminalAuthorityDirectory:
			directory = wrapped.Directory
		case *terminalFaultDirectory:
			directory = wrapped.Directory
		default:
			return directory
		}
	}
}

type stateTerminalNilFileLockDirectory struct{ outputcap.Directory }

func (directory *stateTerminalNilFileLockDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
	if err != nil {
		return nil, created, err
	}
	return &terminalFaultLock{Lock: lock, nilFile: true}, created, nil
}
