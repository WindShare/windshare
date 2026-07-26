package osfs

import (
	"errors"
	"io/fs"
	"testing"
)

func TestOutputV3OpaqueDiscardRequiresLiveEntryAuthority(t *testing.T) {
	failure := errors.New("opaque discard fault")
	for _, test := range []struct {
		name        string
		entry       outputV3EntryRef
		verifyErr   error
		removeErr   error
		wantBytes   uint64
		wantRemoved bool
		cause       error
	}{
		{name: "missing pin", cause: errOutputIntentUnsafe},
		{name: "absent pin", entry: previewEntry(outputV3EntryAbsent, 0), cause: errOutputIntentUnsafe},
		{name: "directory pin", entry: previewEntry(outputV3EntryDirectory, 0), cause: errOutputIntentUnsafe},
		{name: "allocated size failure", entry: &outputV3ResumePreviewEntry{kind: outputV3EntryRegularFile, allocatedErr: failure}, cause: failure},
		{name: "authority changed", entry: previewEntry(outputV3EntryRegularFile, 1), verifyErr: failure, cause: failure},
		{name: "fixed removal failure", entry: previewEntry(outputV3EntryRegularFile, 2), removeErr: failure, cause: failure},
		{name: "opaque regular file", entry: previewEntry(outputV3EntryRegularFile, 7), wantBytes: 7, wantRemoved: true},
		{name: "opaque special entry", entry: previewEntry(outputV3EntryOther, 9), wantBytes: 9, wantRemoved: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := &outputV3ResumePreviewDirectory{names: []string{"remaining"}, removeErr: test.removeErr}
			settlement, err := discardOpaqueSessionEntry(
				nil,
				intent,
				ResumeStateRef{namespaceName: "intent", sessionName: "session"},
				test.entry,
				func() error { return test.verifyErr },
			)
			if test.cause != nil {
				if !errors.Is(err, test.cause) || settlement.Kind != 0 {
					t.Fatalf("opaque discard = (%+v, %v)", settlement, err)
				}
				return
			}
			if err != nil || settlement.Kind != Discarded || settlement.RemovedBytes != test.wantBytes ||
				(intent.removeCalls == 1) != test.wantRemoved {
				t.Fatalf("opaque discard = (%+v, removals=%d, %v)", settlement, intent.removeCalls, err)
			}
		})
	}
}

func TestOutputV3EmptyIntentRemovalRevalidatesPinnedDirectory(t *testing.T) {
	failure := errors.New("empty intent removal fault")
	if err := removeEmptyIntentShell(
		nil, &outputV3ResumePreviewDirectory{namesErr: failure}, "intent",
	); !errors.Is(err, failure) {
		t.Fatalf("intent enumeration failure = %v", err)
	}
	if err := removeEmptyIntentShell(
		nil, &outputV3ResumePreviewDirectory{names: []string{"remaining"}}, "intent",
	); err != nil {
		t.Fatalf("nonempty intent removal = %v", err)
	}

	for _, test := range []struct {
		name       string
		parent     outputV3ResumeCleanupDirectoryFaults
		cause      error
		wantAbsent bool
	}{
		{name: "reopen failure", parent: outputV3ResumeCleanupDirectoryFaults{openErr: failure}, cause: failure},
		{name: "identity mismatch", parent: outputV3ResumeCleanupDirectoryFaults{forceDifferent: true}, cause: errOutputRootUnsafe},
		{name: "identity comparison failure", parent: outputV3ResumeCleanupDirectoryFaults{sameErr: failure}, cause: failure},
		{name: "fixed directory close failure", parent: outputV3ResumeCleanupDirectoryFaults{childCloseErr: failure}, cause: failure},
		{name: "remove failure", parent: outputV3ResumeCleanupDirectoryFaults{removeErr: failure}, cause: failure},
		{name: "already removed", parent: outputV3ResumeCleanupDirectoryFaults{removeErr: fs.ErrNotExist}},
		{name: "parent sync failure", parent: outputV3ResumeCleanupDirectoryFaults{syncErr: failure, delegateRemove: true}, cause: failure, wantAbsent: true},
		{name: "removed", parent: outputV3ResumeCleanupDirectoryFaults{delegateRemove: true}, wantAbsent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, intentName, intent := outputV3ResumeCleanupIntent(t)
			faults := test.parent
			parent := &outputV3ResumeCleanupDirectory{outputV3Directory: session.control.sessions, faults: &faults}
			err := removeEmptyIntentShell(parent, intent, intentName)
			if test.cause != nil {
				if !errors.Is(err, test.cause) {
					t.Fatalf("empty intent removal error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("empty intent removal error = %v", err)
			}
			kind, observeErr := session.control.sessions.ObserveEntry(intentName)
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			if (kind == outputV3EntryAbsent) != test.wantAbsent {
				t.Fatalf("intent absence = %t, want %t", kind == outputV3EntryAbsent, test.wantAbsent)
			}
		})
	}
}

func TestOutputV3EmptySessionShellRequiresVerifiedEmptyIdentity(t *testing.T) {
	failure := errors.New("empty session shell fault")
	entry := previewEntry(outputV3EntryDirectory, 0)
	for _, test := range []struct {
		name      string
		session   *outputV3ResumePreviewDirectory
		intent    *outputV3ResumePreviewDirectory
		verifyErr error
		cause     error
		removed   int
	}{
		{name: "enumeration failure", session: &outputV3ResumePreviewDirectory{namesErr: failure}, intent: &outputV3ResumePreviewDirectory{}, cause: failure},
		{name: "nonempty shell", session: &outputV3ResumePreviewDirectory{names: []string{"remaining"}}, intent: &outputV3ResumePreviewDirectory{}, cause: errOutputIntentUnsafe},
		{name: "authority changed", session: &outputV3ResumePreviewDirectory{}, intent: &outputV3ResumePreviewDirectory{}, verifyErr: failure, cause: failure},
		{name: "fixed session removal failure", session: &outputV3ResumePreviewDirectory{}, intent: &outputV3ResumePreviewDirectory{removeErr: failure}, cause: failure, removed: 1},
		{name: "fixed session already absent", session: &outputV3ResumePreviewDirectory{}, intent: &outputV3ResumePreviewDirectory{removeErr: fs.ErrNotExist, names: []string{"remaining"}}, removed: 1},
		{name: "removed from nonempty intent", session: &outputV3ResumePreviewDirectory{}, intent: &outputV3ResumePreviewDirectory{names: []string{"remaining"}}, removed: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := removeEmptyPinnedSessionShell(
				nil, test.intent, test.session, "intent", "session", entry,
				func() error { return test.verifyErr },
			)
			if test.cause != nil {
				if !errors.Is(err, test.cause) {
					t.Fatalf("empty session shell error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("empty session shell error = %v", err)
			}
			if test.intent.removeCalls != test.removed {
				t.Fatalf("session shell removals = %d, want %d", test.intent.removeCalls, test.removed)
			}
		})
	}
}

func TestOutputV3CorruptDiscardStopsAtEveryAuthorityBoundary(t *testing.T) {
	failure := errors.New("corrupt discard fault")
	for _, test := range []struct {
		name     string
		session  *outputV3ResumePreviewDirectory
		lock     outputV3Lock
		verifyAt int
		cause    error
	}{
		{name: "initial parent verification", session: &outputV3ResumePreviewDirectory{}, verifyAt: 1, cause: failure},
		{name: "private child removal", session: &outputV3ResumePreviewDirectory{openEntryErr: failure}, cause: failure},
		{name: "session enumeration", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0), namesErr: failure}, cause: failure},
		{name: "extra child parent verification", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0), names: []string{"extra"}}, verifyAt: 2, cause: failure},
		{name: "lock parent verification", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0)}, lock: &outputV3ResumeCleanupLock{file: &outputV3ResumePreviewFile{}}, verifyAt: 2, cause: failure},
		{name: "lock without file", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0)}, lock: &outputV3ResumeCleanupLock{}, cause: errOutputIntentUnsafe},
		{name: "lock removal failure", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0), removeFileErr: failure}, lock: &outputV3ResumeCleanupLock{file: &outputV3ResumePreviewFile{}}, cause: failure},
		{name: "lock sync failure", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0), syncErr: failure}, lock: &outputV3ResumeCleanupLock{file: &outputV3ResumePreviewFile{}}, cause: failure},
		{name: "lock close failure", session: &outputV3ResumePreviewDirectory{entry: previewEntry(outputV3EntryAbsent, 0)}, lock: &outputV3ResumeCleanupLock{file: &outputV3ResumePreviewFile{}, closeErr: failure}, cause: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			verifyCalls := 0
			err := recoverCorruptDiscard(
				&outputControlNamespace{}, nil, test.session,
				ResumeStateRef{}, previewEntry(outputV3EntryDirectory, 0), test.lock,
				func() error {
					verifyCalls++
					if verifyCalls == test.verifyAt {
						return failure
					}
					return nil
				},
			)
			if !errors.Is(err, test.cause) {
				t.Fatalf("corrupt discard boundary error = %v", err)
			}
		})
	}
}

type outputV3ResumeCleanupDirectoryFaults struct {
	openErr        error
	forceDifferent bool
	sameErr        error
	childCloseErr  error
	removeErr      error
	delegateRemove bool
	syncErr        error
}

type outputV3ResumeCleanupDirectory struct {
	outputV3Directory
	faults *outputV3ResumeCleanupDirectoryFaults
}

func (directory *outputV3ResumeCleanupDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if directory.faults.openErr != nil {
		return nil, directory.faults.openErr
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3ResumeCleanupDirectory{outputV3Directory: opened, faults: directory.faults}, nil
}

func (directory *outputV3ResumeCleanupDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if directory.faults.sameErr != nil {
		return false, directory.faults.sameErr
	}
	if directory.faults.forceDifferent {
		return false, nil
	}
	if wrapped, ok := other.(*outputV3ResumeCleanupDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *outputV3ResumeCleanupDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	if directory.faults.removeErr != nil {
		return directory.faults.removeErr
	}
	if !directory.faults.delegateRemove {
		return nil
	}
	if wrapped, ok := expected.(*outputV3ResumeCleanupDirectory); ok {
		expected = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.RemoveDirectory(name, expected)
}

func (directory *outputV3ResumeCleanupDirectory) Sync() error {
	if directory.faults.syncErr != nil {
		return directory.faults.syncErr
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3ResumeCleanupDirectory) Close() error {
	return errors.Join(directory.outputV3Directory.Close(), directory.faults.childCloseErr)
}

type outputV3ResumeCleanupLock struct {
	outputV3Lock
	file     outputV3File
	closeErr error
}

func (lock *outputV3ResumeCleanupLock) File() outputV3File { return lock.file }
func (lock *outputV3ResumeCleanupLock) Close() error       { return lock.closeErr }

func outputV3ResumeCleanupIntent(
	t *testing.T,
) (*filesystemOutputSession, string, outputV3Directory) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	intentName := "discard-empty"
	intent, err := opened.Session.control.sessions.CreateDirectory(intentName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(intent.Sync(), opened.Session.control.sessions.Sync()); err != nil {
		_ = intent.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := intent.Close(); err != nil {
			t.Errorf("close empty discard intent: %v", err)
		}
	})
	return opened.Session, intentName, intent
}
