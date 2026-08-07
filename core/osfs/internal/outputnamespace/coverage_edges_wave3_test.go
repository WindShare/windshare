package outputnamespace

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var errWave3Read = errors.New("wave3 read failure")

// These tests exercise deterministic namespace state cuts with the in-memory
// capability backend. They intentionally avoid native filesystems: the point is
// to prove the reducer branches and authority checks, not to duplicate backend
// integration coverage.
func TestWave3BootstrapAndSessionShapeContracts(t *testing.T) {
	expected := []string{
		resumestate.ControlRecordName,
		resumestate.CoordinatorLockName,
		resumestate.SessionsDirectoryName,
	}
	for length := 0; length <= len(expected); length++ {
		got, err := bootstrapCandidatePrefixLength(expected[:length])
		if err != nil || got != length {
			t.Fatalf("bootstrap prefix %d = %d, %v", length, got, err)
		}
	}
	if _, err := bootstrapCandidatePrefixLength([]string{"unknown"}); !errors.Is(err, outputfault.ErrRootUnsafe) {
		t.Fatalf("unknown bootstrap child accepted: %v", err)
	}
	if filtered := bootstrapStructuralNames([]string{
		resumestate.ControlRecordName,
		resumestate.ControlUpdateTemporaryPrefix + "0000000000000000",
		"other",
	}); len(filtered) != 2 || filtered[1] != "other" {
		t.Fatalf("bootstrap structural names = %v", filtered)
	}

	filesystem := newMemoryCapabilityFS(t)
	platform := filesystem.platform()
	defer platform.Close()
	root := platform.Root()
	empty, err := root.CreateDirectory("empty-candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	if inspection, err := inspectBootstrapCandidateStructure(empty); err != nil || inspection.disposition != bootstrapStructureEmpty {
		t.Fatalf("empty bootstrap structure = %+v, %v", inspection, err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	unsafe, err := root.CreateDirectory("unsafe-candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := unsafe.CreateFile("unknown", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unknown.Close()
	if inspection, err := inspectBootstrapCandidateStructure(unsafe); err != nil || inspection.disposition != bootstrapStructureUnsafe {
		t.Fatalf("unsafe bootstrap structure = %+v, %v", inspection, err)
	}
	_ = unsafe.Close()

	selection := v3RecoverySelection(t, false, 0)
	control, err := v3RecoveryAuthority(t, filesystem, nil).newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	ancestry := v3RecoveryAncestryBinding(t, control.OutputRoot(), selection)
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0x51)
	header, err := v3RecoveryAuthority(t, filesystem, nil).newHeader(control, selection, ancestry, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := root.CreateDirectory("session-candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := inspectOutputSessionCandidate(candidate, header); err != nil || state != sessionCandidateEmpty {
		t.Fatalf("empty session candidate = %v, %v", state, err)
	}
	wrong, err := candidate.CreateFile("unexpected", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectOutputSessionCandidate(candidate, header); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("unexpected session child accepted: %v", err)
	}
	if err := candidate.RemoveFile("unexpected", wrong); err != nil {
		t.Fatal(err)
	}
	_ = wrong.Close()
	encodedHeader, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(StoreConfig{Random: bytes.NewReader(bytes.Repeat([]byte{0x71}, 4096))})
	if _, err := store.EnsureInitialRecord(candidate, resumestate.HeaderRecordName, encodedHeader, resumestate.MaxSessionHeaderBytes); err != nil {
		t.Fatal(err)
	}
	lock, err := candidate.CreateFile(resumestate.SessionLockName, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	for _, name := range []string{resumestate.FilesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.StagesDirectoryName} {
		child, createErr := candidate.CreateDirectory(name, true)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_ = child.Close()
	}
	if state, err := inspectOutputSessionCandidate(candidate, header); err != nil || state != sessionCandidateComplete {
		t.Fatalf("complete session candidate = %v, %v", state, err)
	}
	_ = candidate.Close()
}

func TestWave3SessionCreationValidationAndCanonicalIntentEdges(t *testing.T) {
	filesystem := newMemoryCapabilityFS(t)
	platform := filesystem.platform()
	defer platform.Close()
	root := platform.Root()
	controller := v3RecoveryAuthority(t, filesystem, nil)
	selection := v3RecoverySelection(t, false, 0)
	control, err := controller.newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	ancestry := v3RecoveryAncestryBinding(t, control.OutputRoot(), selection)

	sessions, err := root.CreateDirectory("sessions", true)
	if err != nil {
		t.Fatal(err)
	}
	intent := transfer.TransferIntentDigest{0xab}
	opened, err := OpenCanonicalIntent(sessions, intent)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := OpenCanonicalIntent(sessions, intent); err != nil || again == nil {
		t.Fatalf("reopen canonical intent = %v, %v", again, err)
	}
	_ = opened.Close()
	_ = sessions.Close()

	// A sibling that decodes to the same intent but is not the canonical spelling
	// is ambiguous and must stop recovery before any mutation.
	conflictParent, err := root.CreateDirectory("conflict-parent", true)
	if err != nil {
		t.Fatal(err)
	}
	canonicalName := resumestate.IntentNamespaceName(intent)
	conflict, err := conflictParent.CreateDirectory(strings.ToUpper(canonicalName), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = conflict.Close()
	if _, err := OpenCanonicalIntent(conflictParent, intent); err == nil {
		t.Fatal("intent alias was accepted")
	}
	_ = conflictParent.Close()

	intentDirectory, err := root.CreateDirectory("intent", true)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := controller.OpenOrCreateSession(intentDirectory, control, selection, ancestry); err != nil || result.Directory == nil || result.Disposition != SessionInstalled {
		t.Fatalf("new session = %+v, %v", result, err)
	} else {
		_ = result.Directory.Close()
	}
	checkpointFile, err := intentDirectory.CreateFile(resumestate.CheckpointsDirectoryName, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = checkpointFile.Close()
	if _, err := controller.OpenOrCreateSession(intentDirectory, control, selection, ancestry); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("checkpoint wrong kind accepted: %v", err)
	}
	_ = intentDirectory.Close()

	// Creation-cut validation is deliberately prefix based: a later child cannot
	// authorize a missing header, while a header temporary is the sole pre-header cut.
	for _, test := range []struct {
		name  string
		build func(outputcap.Directory) error
		want  error
	}{
		{name: "empty", want: nil},
		{name: "lock-without-header", build: func(directory outputcap.Directory) error {
			file, err := directory.CreateFile(resumestate.SessionLockName, true, 0)
			if err == nil {
				err = file.Close()
			}
			return err
		}, want: outputfault.ErrIntentUnsafe},
		{name: "temporary-only", build: func(directory outputcap.Directory) error {
			nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x2a}, resumestate.UpdateNonceBytes))
			if err != nil {
				return err
			}
			name, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
			if err != nil {
				return err
			}
			file, err := directory.CreateFile(name, true, 1)
			if err == nil {
				err = file.Close()
			}
			return err
		}, want: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMemoryCapabilityFS(t)
			platform := fixture.platform()
			defer platform.Close()
			directory, err := platform.Root().CreateDirectory("cut", true)
			if err != nil {
				t.Fatal(err)
			}
			if test.build != nil {
				if err := test.build(directory); err != nil {
					t.Fatal(err)
				}
			}
			err = validateOutputSessionCandidateCreationCut(directory)
			if !errors.Is(err, test.want) {
				t.Fatalf("creation cut = %v, want %v", err, test.want)
			}
			_ = directory.Close()
		})
	}
}

func TestWave3StateIOAndRecoveryFailureEdges(t *testing.T) {
	for _, test := range []struct {
		name  string
		file  *stateStoreReadFile
		limit int
		want  error
	}{
		{name: "nil", file: nil, limit: 1, want: outputcap.ErrUnsafeNamespace},
		{name: "zero-limit", file: &stateStoreReadFile{size: 1, data: []byte{1}}, limit: 0, want: outputcap.ErrUnsafeNamespace},
		{name: "zero-size", file: &stateStoreReadFile{size: 0}, limit: 1, want: outputcap.ErrUnsafeNamespace},
		{name: "oversized", file: &stateStoreReadFile{size: 2, data: []byte{1, 2}}, limit: 1, want: outputcap.ErrUnsafeNamespace},
		{name: "short-read", file: &stateStoreReadFile{size: 2, data: []byte{1}}, limit: 2, want: io.ErrUnexpectedEOF},
		{name: "read-error", file: &stateStoreReadFile{size: 1, data: []byte{1}, readErr: errWave3Read}, limit: 1, want: errWave3Read},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			if test.file == nil {
				_, err = ReadFile(nil, test.limit)
			} else {
				_, err = ReadFile(test.file, test.limit)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("ReadFile error = %v, want %v", err, test.want)
			}
		})
	}

	filesystem := newMemoryCapabilityFS(t)
	platform := filesystem.platform()
	defer platform.Close()
	root := platform.Root()
	file, err := root.CreateFile("file", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := EnsureDirectory(root, "file", true); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("file accepted as directory: %v", err)
	}
	if result, err := OpenOptionalDirectory(root, "missing", true); err != nil || result.Disposition != DirectoryAbsent {
		t.Fatalf("optional missing directory = %+v, %v", result, err)
	}
	if _, err := OpenOptionalDirectory(root, "file", true); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("optional file accepted: %v", err)
	}
	openErr := errors.New("open")
	failing := &wave2Directory{Directory: root, classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryDirectory, true, nil
	}, open: func(string, bool) (outputcap.Directory, error) { return nil, openErr }}
	if _, err := EnsureDirectory(failing, "present", true); !errors.Is(err, ErrPositiveEntryEvidence) || !errors.Is(err, openErr) {
		t.Fatalf("positive directory evidence = %v", err)
	}
	if _, err := OpenOptionalDirectory(failing, "present", true); !errors.Is(err, ErrPositiveEntryEvidence) || !errors.Is(err, openErr) {
		t.Fatalf("optional positive evidence = %v", err)
	}
	if _, err := ObserveExactEntry(&wave2Directory{Directory: root, classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryRegularFile, false, nil
	}}, "alias"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("inexact entry accepted: %v", err)
	}

	// Recovery classification distinguishes missing, malformed, and regular
	// temporary entries before reducers are allowed to inspect bytes.
	for _, kind := range []outputcap.EntryKind{outputcap.EntryAbsent, outputcap.EntryRegularFile, outputcap.EntryDirectory, outputcap.EntryOther} {
		_ = classifyHeaderTemporaryEntry(kind)
	}
	if got := classifyHeaderTemporaryEntry(outputcap.EntryAbsent); got != resumestate.UpdateTemporaryEntryMissing {
		t.Fatalf("missing temporary classification = %v", got)
	}
	if got := classifyHeaderTemporaryEntry(outputcap.EntryOther); got != resumestate.UpdateTemporaryEntryUnsafe {
		t.Fatalf("other temporary classification = %v", got)
	}
}

func TestWave3TerminalPublicationAndCleanupEdges(t *testing.T) {
	for cut := range len(terminalRemovalOrder) {
		nameSet := append([]string(nil), terminalRemovalOrder[cut:]...)
		got, ok := terminalPublicationCut(nameSet)
		if !ok || got != cut {
			t.Fatalf("terminal cut %d = %d/%t", cut, got, ok)
		}
	}
	if _, ok := terminalPublicationCut([]string{"unexpected"}); ok {
		t.Fatal("invalid terminal cut accepted")
	}
	if err := closeLock(nil); err != nil {
		t.Fatal(err)
	}
	if err := classifyTerminalLockFailure(transfer.OutputFaultSession, errors.New("state")); err == nil {
		t.Fatal("terminal state failure lost")
	}
	filesystem := newMemoryCapabilityFS(t)
	platform := filesystem.platform()
	defer platform.Close()
	empty, err := platform.Root().CreateDirectory("terminal-empty", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareTerminalDirectoryForRemoval(empty, false, func() error { return nil }); err != nil {
		t.Fatalf("empty completing directory = %v", err)
	}
	child, err := empty.CreateFile("child", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	if err := prepareTerminalDirectoryForRemoval(empty, false, func() error { return nil }); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("non-empty completing directory = %v", err)
	}
	if err := prepareTerminalDirectoryForRemoval(empty, true, func() error { return nil }); err != nil {
		t.Fatalf("discarded directory cleanup = %v", err)
	}
	_ = empty.Close()
	if _, err := validateTerminalLayoutLock(nil); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("nil terminal lock = %v", err)
	}
	fixture := newTerminalNamespaceFixture(t, resumestate.SessionCompleting, true)
	defer fixture.close(t)
	if err := RemoveEmptySessionShell(
		fixture.sessionsDirectory,
		fixture.intentDirectory,
		fixture.sessionDirectory,
		fixture.intentName,
		fixture.sessionName,
	); err == nil {
		// The completing shell still contains its header until terminal retirement;
		// this call must fail closed rather than remove a live session.
		t.Fatal("non-empty session shell was removed")
	}
	if err := removeAuthorizedHeaderTemporary(nil, resumestate.SessionNamespaceAuthority{}, "", nil, resumestate.HeaderUpdateTemporaryDecision{}, func() error { return errors.New("authority") }); err == nil {
		t.Fatal("invalid temporary removal authority accepted")
	}
	if err := errors.Join(fixture.sessionDirectory.Close(), fixture.intentDirectory.Close()); err != nil && !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("close terminal wrappers = %v", err)
	}
}
