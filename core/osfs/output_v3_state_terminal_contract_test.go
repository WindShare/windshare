package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestOutputV3ProbeObservationParsesOnlyTheNativeProbeVocabulary(t *testing.T) {
	observation := &outputV3ProbeCutObservation{}
	for name, size := range map[string]uint64{
		"stage": 0, "anchor": 1, "publication": 1, "record": 0, "record.tmp": 1,
	} {
		if err := observation.observeFile(name, size); err != nil {
			t.Fatalf("observe probe file %q: %v", name, err)
		}
	}
	if !observation.stage.present || !observation.anchor.present ||
		!observation.publication.present || !observation.record.present ||
		!observation.temporary.present {
		t.Fatalf("probe file observation is incomplete: %+v", observation)
	}
	for _, name := range []string{"candidate", "installed"} {
		if err := observation.observeDirectory(name); err != nil {
			t.Fatalf("observe probe directory %q: %v", name, err)
		}
	}
	if !observation.candidate || !observation.installed {
		t.Fatalf("probe directory observation is incomplete: %+v", observation)
	}

	if err := (*outputV3ProbeCutObservation)(nil).observeFile("stage", 0); err == nil {
		t.Fatal("nil probe observation accepted a file")
	}
	if err := (*outputV3ProbeCutObservation)(nil).observeDirectory("candidate"); err == nil {
		t.Fatal("nil probe observation accepted a directory")
	}
	if err := observation.observeFile("foreign", 0); err == nil {
		t.Fatal("unknown probe file was accepted")
	}
	if err := observation.observeDirectory("foreign"); err == nil {
		t.Fatal("unknown probe directory was accepted")
	}
	if err := validateOutputV3ProbeCut(
		outputV3ProbeDataWindowsNTFS,
		outputV3ProbeCutObservation{stage: outputV3ProbeObservedFile{present: true, size: 2}},
	); err == nil {
		t.Fatal("oversized Windows probe stage was accepted")
	}
}

func TestOutputV3StateStoreRejectsUnprovableCreationAndReplacementCuts(t *testing.T) {
	current, next, _ := stateStoreHeaderImages(t)

	t.Run("invalid-create-image", func(t *testing.T) {
		if _, err := (outputStateStore{}).createRecord(nil, "state", nil, 1); err == nil {
			t.Fatal("empty state image reached the filesystem")
		}
	})

	t.Run("replacement-nonce-source", func(t *testing.T) {
		outcome, err := (outputStateStore{random: bytes.NewReader(nil)}).replaceRecord(
			nil, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputStateReplaceUnchanged || err == nil {
			t.Fatalf("nonce failure = (%v, %v), want unchanged error", outcome, err)
		}
	})

	t.Run("replacement-collision-budget", func(t *testing.T) {
		faults := &stateStoreFaultDirectory{fault: stateStoreFaultCreateCollision}
		outcome, err := (outputStateStore{
			random: bytes.NewReader(bytes.Repeat([]byte{0x31}, 2048)),
		}).replaceRecord(
			faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputStateReplaceUnchanged || err == nil {
			t.Fatalf("collision exhaustion = (%v, %v), want unchanged error", outcome, err)
		}
	})

	t.Run("replacement-short-write", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, current.encoded)
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreFaultDirectory{
			outputV3Directory: directory,
			fault:             stateStoreFaultShortWrite,
			target:            resumestate.HeaderRecordName,
		}
		outcome, err := (outputStateStore{
			random: bytes.NewReader(bytes.Repeat([]byte{0x32}, 128)),
		}).replaceRecord(
			faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
		)
		if outcome != outputStateReplaceUnchanged || err == nil {
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
				outputV3Directory: directory,
				target:            resumestate.HeaderRecordName,
				sameFileMismatch:  test.sameFileMismatch,
				targetOpenErr:     test.targetOpenErr,
			}
			_, err := (outputStateStore{
				random: bytes.NewReader(bytes.Repeat([]byte{0x33}, 128)),
			}).createRecord(faults, resumestate.HeaderRecordName, []byte("image"), 5)
			if err == nil {
				t.Fatal("unproved installed state was accepted")
			}
		})
	}
}

func TestOutputV3StateRecoveryRejectsAmbiguousObservationBoundaries(t *testing.T) {
	t.Run("initial-target-observation", func(t *testing.T) {
		directory := &stateTerminalClassifyDirectory{classifyErr: errStateTerminalInjected}
		if _, err := (outputStateStore{}).ensureInitialRecord(directory, "record", []byte{1}, 1); !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("initial observation error = %v", err)
		}
	})

	t.Run("header-reconcile-requires-authority", func(t *testing.T) {
		if err := reconcileHeaderRecordTemporaries(nil, resumestate.SessionNamespaceAuthority{}, nil); err == nil {
			t.Fatal("header reconciliation accepted absent authority verifier")
		}
		if err := reconcileHeaderRecordTemporaries(
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
		err := reconcileHeaderRecordTemporaries(
			session.sessionDir, namespace, func() error { return errStateTerminalInjected },
		)
		if !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("installed authority error = %v", err)
		}
	})

	t.Run("temporary-enumeration", func(t *testing.T) {
		directory := &stateStoreReconcileFaultDirectory{
			outputV3Directory: session.sessionDir,
			namesErr:          errStateTerminalInjected,
		}
		err := reconcileHeaderRecordTemporaries(directory, namespace, func() error { return nil })
		if !errors.Is(err, errStateTerminalInjected) {
			t.Fatalf("temporary enumeration error = %v", err)
		}
	})

	temporaryName, err := session.store.temporaryName(resumestate.HeaderRecordName)
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
				outputV3Directory: session.sessionDir,
				namesOverride:     []string{temporaryName},
				classifyOverride:  true,
				classifyName:      temporaryName,
				classifyKind:      outputV3EntryRegularFile,
				classifyErr:       test.classifyErr,
				openName:          temporaryName,
				openErr:           test.openErr,
			}
			err := reconcileHeaderRecordTemporaries(directory, namespace, func() error { return nil })
			if !errors.Is(err, errStateTerminalInjected) {
				t.Fatalf("temporary inspection error = %v", err)
			}
		})
	}

	if _, err := readStateRecord(
		&stateTerminalClassifyDirectory{classifyErr: errStateTerminalInjected}, "record", 1,
	); !errors.Is(err, errStateTerminalInjected) {
		t.Fatalf("state record observation error = %v", err)
	}
	if _, err := readStateRecord(
		&stateTerminalClassifyDirectory{kind: outputV3EntryDirectory, exact: true}, "record", 1,
	); err == nil {
		t.Fatal("directory was accepted as a state record")
	}
	if _, err := observeExactOutputEntry(
		&stateTerminalClassifyDirectory{kind: outputV3EntryRegularFile}, "record",
	); err == nil {
		t.Fatal("non-exact state entry was accepted")
	}
}

func TestOutputV3TerminalRecoveryRevalidatesAuthorityAtEveryRemovalBoundary(t *testing.T) {
	for _, failAt := range []int{1, 2, 7, 8, 9, 10} {
		t.Run(fmt.Sprintf("authority-check-%d", failAt), func(t *testing.T) {
			fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
			defer fixture.close(t)
			counter := &stateTerminalAuthorityDirectory{
				outputV3Directory: fixture.control.sessions,
				failAt:            failAt,
			}
			fixture.control.sessions = counter
			err := recoverTerminalNamespace(
				fixture.control, fixture.intentDirectory, fixture.sessionDirectory,
				fixture.header, fixture.layout, true,
			)
			if !errors.Is(err, errStateTerminalInjected) || counter.openCalls != failAt {
				t.Fatalf("authority cut %d = (%v, calls=%d)", failAt, err, counter.openCalls)
			}
		})
	}
}

func TestOutputV3TerminalShellAndLockObservationFailClosed(t *testing.T) {
	t.Run("session-binding-before-shell-removal", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		counter := &stateTerminalAuthorityDirectory{
			outputV3Directory: fixture.intentDirectory,
			failAt:            9,
		}
		control := *fixture.control
		control.sessions = &stateTerminalAuthorityDirectory{outputV3Directory: fixture.control.sessions}
		err := recoverTerminalNamespace(
			&control, counter, fixture.sessionDirectory,
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
		err := recoverTerminalNamespace(
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
			outputV3Directory: fixture.intentDirectory,
			removeMissing:     true,
		}
		control := *fixture.control
		control.sessions = &stateTerminalAuthorityDirectory{outputV3Directory: fixture.control.sessions}
		if err := recoverTerminalNamespace(
			&control, intent, fixture.sessionDirectory,
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
		layout, err := inspectTerminalSessionLayout(
			&stateTerminalNilFileLockDirectory{outputV3Directory: session.sessionDir},
			session.state.Header(),
		)
		if layout != nil {
			_ = layout.close()
		}
		if err == nil {
			t.Fatal("terminal layout accepted a lock without file authority")
		}
	})

	t.Run("header-reread", func(t *testing.T) {
		fixture := newTerminalRecoveryFaultFixture(t, resumestate.SessionDiscarding)
		defer fixture.close(t)
		sessionDirectory := &terminalFaultDirectory{
			outputV3Directory: unwrapStateTerminalDirectory(fixture.sessionDirectory),
			openFileName:      resumestate.HeaderRecordName,
			forceOpenFileErr:  true,
		}
		err := verifyTerminalSessionAuthority(
			fixture.control, fixture.intentDirectory, sessionDirectory, fixture.header,
		)
		if !errors.Is(err, errTerminalRecoveryInjected) {
			t.Fatalf("terminal header reread error = %v", err)
		}
	})

	if err := (*outputTerminalLayout)(nil).close(); err != nil {
		t.Fatalf("close nil terminal layout: %v", err)
	}
}

var errStateTerminalInjected = errors.New("injected state/terminal authority cut")

type stateTerminalClassifyDirectory struct {
	outputV3Directory
	kind        outputV3EntryKind
	exact       bool
	classifyErr error
}

func (directory *stateTerminalClassifyDirectory) ClassifyExactEntry(
	string,
) (outputV3EntryKind, bool, error) {
	return directory.kind, directory.exact, directory.classifyErr
}

type stateTerminalCreateDirectory struct {
	outputV3Directory
	target           string
	sameFileMismatch bool
	targetOpenErr    bool
}

func (directory *stateTerminalCreateDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if name == directory.target && directory.targetOpenErr {
		return nil, errStateTerminalInjected
	}
	return directory.outputV3Directory.OpenFile(name, private, writable)
}

func (directory *stateTerminalCreateDirectory) LinkFileNoReplace(
	source outputV3File,
	name string,
) (outputV3File, error) {
	linked, err := directory.outputV3Directory.LinkFileNoReplace(source, name)
	if err != nil || !directory.sameFileMismatch {
		return linked, err
	}
	return &stateTerminalDifferentFile{outputV3File: linked}, nil
}

type stateTerminalDifferentFile struct{ outputV3File }

func (*stateTerminalDifferentFile) SameFile(outputV3File) (bool, error) { return false, nil }

type stateTerminalAuthorityDirectory struct {
	outputV3Directory
	openCalls     int
	failAt        int
	removeMissing bool
}

func (directory *stateTerminalAuthorityDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	directory.openCalls++
	if directory.openCalls == directory.failAt {
		return nil, errStateTerminalInjected
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &stateTerminalAuthorityDirectory{outputV3Directory: opened}, nil
}

func (directory *stateTerminalAuthorityDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	if directory.removeMissing {
		return fs.ErrNotExist
	}
	return directory.outputV3Directory.RemoveDirectory(name, unwrapStateTerminalDirectory(expected))
}

func (directory *stateTerminalAuthorityDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return unwrapStateTerminalDirectory(directory.outputV3Directory).SameDirectory(
		unwrapStateTerminalDirectory(other),
	)
}

func unwrapStateTerminalDirectory(directory outputV3Directory) outputV3Directory {
	for {
		switch wrapped := directory.(type) {
		case *stateTerminalAuthorityDirectory:
			directory = wrapped.outputV3Directory
		case *terminalFaultDirectory:
			directory = wrapped.outputV3Directory
		default:
			return directory
		}
	}
}

type stateTerminalNilFileLockDirectory struct{ outputV3Directory }

func (directory *stateTerminalNilFileLockDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	lock, created, err := directory.outputV3Directory.AcquireLock(name, existingOnly)
	if err != nil {
		return nil, created, err
	}
	return &terminalFaultLock{outputV3Lock: lock, nilFile: true}, created, nil
}
