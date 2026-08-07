package outputnamespace

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestWave2EmptyIntentMetadataRemovalUsesPinnedEmptyAuthority(t *testing.T) {
	filesystem := newMemoryCapabilityFS(t)
	root := filesystem.platform().Root()
	defer root.Close()

	if err := removeEmptyIntentMetadataDirectory(root, "missing", func() error { return nil }); err != nil {
		t.Fatalf("missing metadata child = %v", err)
	}

	empty, err := root.CreateDirectory("empty", true)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	verified := 0
	if err := removeEmptyIntentMetadataDirectory(root, "empty", func() error {
		verified++
		return nil
	}); err != nil {
		t.Fatalf("empty metadata removal = %v", err)
	}
	if verified != 2 {
		t.Fatalf("empty metadata verification count = %d", verified)
	}
	if kind, err := root.ObserveEntry("empty"); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("removed metadata observation = %v, %v", kind, err)
	}

	nonempty, err := root.CreateDirectory("nonempty", true)
	if err != nil {
		t.Fatal(err)
	}
	child, err := nonempty.CreateDirectory("record", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	_ = nonempty.Close()
	if err := removeEmptyIntentMetadataDirectory(root, "nonempty", func() error { return nil }); err != nil {
		t.Fatalf("nonempty metadata inspection = %v", err)
	}
	if kind, err := root.ObserveEntry("nonempty"); err != nil || kind != outputcap.EntryDirectory {
		t.Fatalf("nonempty metadata was removed: %v, %v", kind, err)
	}

	wrongKind, err := root.CreateFile("file", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongKind.Close()
	if err := removeEmptyIntentMetadataDirectory(root, "file", func() error { return nil }); err == nil || !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("file metadata child = %v", err)
	}

	failedVerification, err := root.CreateDirectory("verify-failure", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = failedVerification.Close()
	verifyErr := errors.New("injected intent verification failure")
	if err := removeEmptyIntentMetadataDirectory(root, "verify-failure", func() error { return verifyErr }); !errors.Is(err, verifyErr) {
		t.Fatalf("intent verification failure = %v", err)
	}

	replaced, err := root.CreateDirectory("replaced", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = replaced.Close()
	replacedOnce := false
	if err := removeEmptyIntentMetadataDirectory(root, "replaced", func() error {
		if !replacedOnce {
			filesystem.mu.Lock()
			filesystem.root.children["replaced"] = filesystem.newNode(outputcap.EntryDirectory)
			filesystem.mu.Unlock()
			replacedOnce = true
		}
		return nil
	}); err == nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replaced metadata identity = %v", err)
	}
}

func TestWave2PrivateEntryRemovalCoversEveryEntryClass(t *testing.T) {
	filesystem := newMemoryCapabilityFS(t)
	root := filesystem.platform().Root()
	defer root.Close()
	verifyCalls := 0
	verify := func() error {
		verifyCalls++
		return nil
	}

	if err := removePrivateEntry(root, "missing", 0, verify); err != nil {
		t.Fatalf("missing private entry = %v", err)
	}
	file, err := root.CreateFile("file", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := removePrivateEntry(root, "file", 0, verify); err != nil {
		t.Fatalf("private file removal = %v", err)
	}

	nested, err := root.CreateDirectory("nested", true)
	if err != nil {
		t.Fatal(err)
	}
	nestedFile, err := nested.CreateFile("part", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = nestedFile.Close()
	_ = nested.Close()
	if err := removePrivateEntry(root, "nested", 0, verify); err != nil {
		t.Fatalf("nested private removal = %v", err)
	}

	filesystem.mu.Lock()
	filesystem.root.children["other"] = filesystem.newNode(outputcap.EntryOther)
	filesystem.mu.Unlock()
	if err := removePrivateEntry(root, "other", 0, verify); err != nil {
		t.Fatalf("other private entry removal = %v", err)
	}
	if verifyCalls < 3 {
		t.Fatalf("private removal authority checks = %d", verifyCalls)
	}

	if err := RemovePrivateDirectoryContents(root, resumestate.MaxStateNestingDepth+1, verify); !errors.Is(err, outputfault.ErrInspectionLimit) {
		t.Fatalf("private removal depth limit = %v", err)
	}
	failingFile, err := root.CreateFile("verify-failure", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = failingFile.Close()
	verifyErr := errors.New("injected private removal verification failure")
	if err := removePrivateEntry(root, "verify-failure", 0, func() error { return verifyErr }); !errors.Is(err, verifyErr) {
		t.Fatalf("private removal verification failure = %v", err)
	}

	pinnedFailure, err := root.CreateDirectory("pinned-failure", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = pinnedFailure.Close()
	openErr := errors.New("injected pinned-directory open failure")
	failingPinnedOpen := &wave2Directory{
		Directory: root,
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return nil, openErr
		},
	}
	if err := removePrivateEntry(failingPinnedOpen, "pinned-failure", 0, verify); !errors.Is(err, openErr) {
		t.Fatalf("pinned directory open failure = %v", err)
	}

	replacedDirectory, err := root.CreateDirectory("replaced-directory", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = replacedDirectory.Close()
	replacedOnce := false
	if err := removePrivateEntry(root, "replaced-directory", 0, func() error {
		if !replacedOnce {
			filesystem.mu.Lock()
			filesystem.root.children["replaced-directory"] = filesystem.newNode(outputcap.EntryDirectory)
			filesystem.mu.Unlock()
			replacedOnce = true
		}
		return nil
	}); err == nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("replaced private directory = %v", err)
	}

	lateFailure, err := root.CreateDirectory("late-verification", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = lateFailure.Close()
	verification := 0
	if err := removePrivateEntry(root, "late-verification", 0, func() error {
		verification++
		if verification == 2 {
			return verifyErr
		}
		return nil
	}); !errors.Is(err, verifyErr) {
		t.Fatalf("late private directory verification = %v", err)
	}

	filesystem.mu.Lock()
	filesystem.root.children["invalid-kind"] = filesystem.newNode(outputcap.EntryKind(255))
	filesystem.mu.Unlock()
	if err := removePrivateEntry(root, "invalid-kind", 0, verify); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("invalid private entry kind = %v", err)
	}

	// A vanished entry is a settled absence, not a state-I/O failure.
	if err := removePrivateEntry(root, "vanished", 0, func() error { return fs.ErrNotExist }); err != nil {
		t.Fatalf("vanished private entry = %v", err)
	}
}

func TestWave2StateIOFailureEvidenceAndObserverBinding(t *testing.T) {
	filesystem := newMemoryCapabilityFS(t)
	root := filesystem.platform().Root()
	defer root.Close()

	observeErr := errors.New("injected classify failure")
	failingClassify := &wave2Directory{
		Directory: root,
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, observeErr
		},
	}
	if result := ReadRecordWithCleanup(failingClassify, "record", 64); !errors.Is(result.ReadError, observeErr) {
		t.Fatalf("record observation failure = %+v", result)
	}
	if _, err := root.CreateDirectory("directory-record", true); err != nil {
		t.Fatal(err)
	}
	if result := ReadRecordWithCleanup(root, "directory-record", 64); !errors.Is(result.ReadError, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("directory record classification = %+v", result)
	}

	store := NewStore(StoreConfig{Random: bytes.NewReader(bytes.Repeat([]byte{0x22}, 4_096))})
	payload := []byte("wave2-state")
	stateName := resumestate.HeaderRecordName
	if outcome, err := store.EnsureInitialRecord(root, stateName, payload, 64); err != nil || outcome != CreateAdopted {
		t.Fatalf("initial record = %v, %v", outcome, err)
	}
	if err := VerifyRecord(root, stateName, payload, 64); err != nil {
		t.Fatalf("exact record verification = %v", err)
	}
	if err := VerifyRecord(root, stateName, []byte("different"), 64); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("mismatched record verification = %v", err)
	}
	if err := VerifyRecord(root, "missing", payload, 64); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing record verification = %v", err)
	}

	existing, err := root.CreateDirectory("existing", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = existing.Close()
	result, err := EnsureDirectory(root, "existing", true)
	if err != nil || result.Disposition != DirectoryExisting || result.Directory == nil {
		t.Fatalf("existing directory = %+v, %v", result, err)
	}
	_ = result.Directory.Close()

	openErr := errors.New("injected directory open failure")
	failingOpen := &wave2Directory{
		Directory: root,
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryDirectory, true, nil
		},
		open: func(string, bool) (outputcap.Directory, error) { return nil, openErr },
	}
	if _, err := EnsureDirectory(failingOpen, "existing", true); !errors.Is(err, ErrPositiveEntryEvidence) || !errors.Is(err, openErr) {
		t.Fatalf("existing directory open failure = %v", err)
	}
	if _, err := OpenOptionalDirectory(failingOpen, "existing", true); !errors.Is(err, ErrPositiveEntryEvidence) || !errors.Is(err, openErr) {
		t.Fatalf("optional directory open failure = %v", err)
	}

	alias := &wave2Directory{
		Directory: root,
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, false, nil
		},
	}
	if _, err := ObserveExactEntry(alias, "alias"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("inexact entry observation = %v", err)
	}

	intent := transfer.TransferIntentDigest{1}
	sessionID := transfer.OutputSessionID{2}
	var observed StateInstallEvent
	controller := NewController(ControllerConfig{
		Random: bytes.NewReader(bytes.Repeat([]byte{0x33}, 64)),
		Observer: ObserverFunc(func(event StateInstallEvent) {
			observed = event
		}),
	})
	observedStore := controller.Store(intent, sessionID)
	observedStore.observer.ObserveStateInstall(StateInstallCut{stage: StateInstallCreate, targetName: "record"})
	if observed.IntentDigest != intent || observed.SessionID != sessionID || observed.Cut.TargetName() != "record" {
		t.Fatalf("bound state observation = %+v", observed)
	}

	if err := RootFault("probe", outputcap.ErrRecoverableOutputUnsupported); !errors.Is(err, outputfault.ErrUnsupportedVolume) {
		t.Fatalf("unsupported root fault = %v", err)
	}
	if err := RootFault("probe", errors.New("unsafe")); !errors.Is(err, outputfault.ErrRootUnsafe) {
		t.Fatalf("unsafe root fault = %v", err)
	}
	if err := classifyTerminalLockFailure(transfer.OutputFaultSession, outputcap.ErrNamespaceLockBusy); !errors.Is(err, outputfault.ErrSessionActive) {
		t.Fatalf("busy terminal lock fault = %v", err)
	}
}

func TestWave2TerminalOptionalAuthoritiesFailClosed(t *testing.T) {
	filesystem := newMemoryCapabilityFS(t)
	root := filesystem.platform().Root()
	defer root.Close()

	var absent outputcap.Directory
	if err := retireTerminalDirectory(root, "absent", &absent, false, func() error { return nil }); err != nil {
		t.Fatalf("absent terminal directory = %v", err)
	}
	if err := retireTerminalLock(root, &TerminalLayout{}, func() error { return nil }); err != nil {
		t.Fatalf("absent terminal lock = %v", err)
	}
	if err := openTerminalDirectories(root, nil, &TerminalLayout{}); err != nil {
		t.Fatalf("empty terminal directory set = %v", err)
	}
	lockErr := errors.New("injected terminal lock acquisition failure")
	if _, err := acquireTerminalLayoutLock(root, func() (outputcap.Lock, error) { return nil, lockErr }); !errors.Is(err, lockErr) {
		t.Fatalf("terminal lock callback failure = %v", err)
	}
	if _, err := validateTerminalLayoutLock(nil); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("nil terminal lock = %v", err)
	}
	stateErr := errors.New("injected terminal state failure")
	if err := classifyTerminalLockFailure(transfer.OutputFaultSession, stateErr); !errors.Is(err, stateErr) {
		t.Fatalf("terminal state fault = %v", err)
	}
}

type wave2Directory struct {
	outputcap.Directory
	classify   func(string) (outputcap.EntryKind, bool, error)
	open       func(string, bool) (outputcap.Directory, error)
	openPinned func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error)
}

func (directory *wave2Directory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory.classify != nil {
		return directory.classify(name)
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *wave2Directory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.open != nil {
		return directory.open(name, private)
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *wave2Directory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	if directory.openPinned != nil {
		return directory.openPinned(expected, private)
	}
	return directory.Directory.OpenPinnedDirectory(expected, private)
}
