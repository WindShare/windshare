package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3PublicAuthorityInventoryRoundTrip(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := v3OpenSelection(context.Background(), authority, selection)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)

	inventory, err := ListResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("resume summaries = %+v, want one recoverable session", summaries)
	}
	reference := summaries[0].Reference
	if reference.ResumeIntent() != selection.ResumeIntent() ||
		reference.SessionID() != opened.Session.SessionID() ||
		reference.Kind() != ResumeStateRecoverable {
		t.Fatalf("public reference metadata = intent %s session %s kind %v",
			reference.ResumeIntent(), reference.SessionID(), reference.Kind())
	}
	settlement, err := DiscardResumeState(context.Background(), reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard public resume reference = (%+v, %v)", settlement, err)
	}
}

func TestOutputV3NestedDirectoryLifecycleAndPreObjectCollision(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3SemanticNestedSelection(t, 23)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	session := opened.Session
	closed := false
	defer func() {
		if !closed {
			v3RecoveryCloseSession(t, session)
		}
	}()

	selectedDirectory := selection.Directories()[0]
	directory := transfer.OutputDirectory{Path: selectedDirectory.Path, ModifiedTime: selectedDirectory.ModifiedTime}
	if err := session.FinalizeDirectory(context.Background(), directory); err != nil {
		t.Fatalf("finalize admitted nested directory: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.FinalizeDirectory(canceled, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("finalize with canceled context = %v, want cancellation", err)
	}
	if err := (*filesystemOutputSession)(nil).FinalizeDirectory(context.Background(), directory); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session finalize = %v, want invalid binding", err)
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "unselected"}); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("unselected directory finalize = %v, want invalid selection", err)
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: directory.Path}); !errors.Is(err, transfer.ErrInvalidOutputSelection) {
		t.Fatalf("mismatched directory finalize = %v, want invalid selection", err)
	}

	file := v3RecoveryOutputFile(t, session, selection, 23)
	finalPath := filepath.Join(root, filepath.FromSlash(file.Path))
	foreign := []byte("pre-existing")
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("begin colliding file: %v", err)
	}
	collision, ok := start.ImmediateSettlement()
	if !ok || collision.Kind() != transfer.FileCollision {
		t.Fatalf("pre-object collision settlement = (%+v, %t), want collision", collision, ok)
	}
	if actual, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(actual, foreign) {
		t.Fatalf("collision changed foreign final = %q, %v", actual, err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	start, err = session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatalf("begin admitted nested file: %v", err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || len(durable.Ranges().Ranges()) != 0 {
		t.Fatalf("new nested file start = transaction %t ranges %v", ok, durable.Ranges().Ranges())
	}
	payload := bytes.Repeat([]byte{0x6d}, int(file.ExpectedSize))
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || !transfer.RangesCoverFile(file.ExpectedSize, checkpoint.Ranges()) {
		t.Fatalf("checkpoint nested file = ranges %v, err %v", checkpoint.Ranges().Ranges(), err)
	}
	published, err := transaction.Commit(context.Background())
	if err != nil || published.Kind() != transfer.FilePublished {
		t.Fatalf("publish nested file = (%+v, %v)", published, err)
	}
	job, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || job.Kind() != transfer.JobClosed {
		t.Fatalf("complete nested output session = (%+v, %v)", job, err)
	}
	closed = true
	if actual, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("published nested file = %x, %v", actual, err)
	}
}

func TestOutputV3AuthorityPrimitivesRejectTamperingAndReleasePins(t *testing.T) {
	if inventory := (*ResumeStateInventory)(nil); inventory.Summaries() != nil {
		t.Fatal("nil inventory returned summaries")
	} else if err := inventory.Close(); err != nil {
		t.Fatalf("close nil inventory: %v", err)
	}
	if pin := newResumeStateEntryPin(nil); pin != nil {
		t.Fatalf("nil entry produced pin %+v", pin)
	}
	if (*resumeStateEntryPin)(nil).available() || (*resumeStateEntryPin)(nil).take() != nil {
		t.Fatal("nil entry pin granted authority")
	}
	if err := (*resumeStateEntryPin)(nil).Close(); err != nil {
		t.Fatalf("close nil entry pin: %v", err)
	}

	entry := &v3SemanticEntryRef{kind: outputV3EntryDirectory}
	pin := newResumeStateEntryPin(entry)
	if !pin.available() || pin.take() != entry || pin.available() || pin.take() != nil {
		t.Fatal("entry pin did not transfer exactly once")
	}
	if err := pin.Close(); err != nil || entry.closes.Load() != 0 {
		t.Fatalf("consumed pin close = %v, underlying closes %d", err, entry.closes.Load())
	}

	closeCause := errors.New("close pin")
	closing := &v3SemanticEntryRef{kind: outputV3EntryRegularFile, closeErr: closeCause}
	closingPin := newResumeStateEntryPin(closing)
	if err := closingPin.Close(); !errors.Is(err, closeCause) || closing.closes.Load() != 1 {
		t.Fatalf("owned pin close = %v, closes %d", err, closing.closes.Load())
	}
	if err := closingPin.Close(); err != nil || closing.closes.Load() != 1 {
		t.Fatalf("second pin close = %v, closes %d", err, closing.closes.Load())
	}
	platform, err := openOutputV3Platform(v3RecoveryRoot(t), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	duplicate, err := platform.Root().Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	countingRoot := &v3SemanticCountingDirectory{outputV3Directory: duplicate}
	rootPin := newResumeStateDirectoryPin(countingRoot)
	if !rootPin.available() || rootPin.fixedDirectory() != countingRoot || !rootPin.retain() {
		t.Fatal("shared root pin did not retain fixed directory authority")
	}
	if err := rootPin.Close(); err != nil || countingRoot.closes.Load() != 0 {
		t.Fatalf("release retained root reference = %v, closes %d", err, countingRoot.closes.Load())
	}
	if err := rootPin.Close(); err != nil || countingRoot.closes.Load() != 1 || rootPin.available() {
		t.Fatalf("release final root reference = %v, closes %d", err, countingRoot.closes.Load())
	}
	if err := rootPin.forceClose(); err != nil || countingRoot.closes.Load() != 1 {
		t.Fatalf("force close released root pin = %v, closes %d", err, countingRoot.closes.Load())
	}

	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	firstSession, err := authority.sessionIDs.NewOutputSessionID()
	if err != nil || firstSession.IsZero() {
		t.Fatalf("cryptographic session ID = %s, %v", firstSession, err)
	}
	secondSession, err := authority.sessionIDs.NewOutputSessionID()
	if err != nil || secondSession.IsZero() || secondSession == firstSession {
		t.Fatalf("second cryptographic session ID = %s, %v", secondSession, err)
	}
	objectID, err := authority.objectIDs.NewOutputObjectID()
	if err != nil || objectID.IsZero() {
		t.Fatalf("cryptographic object ID = %s, %v", objectID, err)
	}

	var traced atomic.Int64
	FilesystemOutputTraceFunc(func(FilesystemOutputTrace) { traced.Add(1) }).TraceFilesystemOutput(FilesystemOutputTrace{})
	FilesystemOutputTraceFunc(nil).TraceFilesystemOutput(FilesystemOutputTrace{})
	if traced.Load() != 1 {
		t.Fatalf("trace callback count = %d", traced.Load())
	}
	authority.tracer = FilesystemOutputTraceFunc(func(FilesystemOutputTrace) { traced.Add(1) })
	authority.trace(FilesystemOutputTrace{})
	(*FilesystemOutputAuthority)(nil).trace(FilesystemOutputTrace{})
	if traced.Load() != 2 {
		t.Fatalf("authority trace callback count = %d", traced.Load())
	}
}

func v3SemanticNestedSelection(t *testing.T, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x41)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x42)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x43)
	directory := transfer.OutputSelectionDirectory{
		Path: "folder", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x44),
		Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x45), ModifiedTime: v3RecoveryModifiedTime(t),
	}
	file := transfer.OutputSelectionFile{
		Path: "folder/file.bin", FileID: v3RecoveryIdentity16[catalog.FileID](0x46),
		ParentDirectoryID: directory.DirectoryID, ParentGeneration: directory.Generation,
		ExpectedSize: exactSize, ModifiedTime: v3RecoveryModifiedTime(t),
	}
	plan, err := transfer.NewOutputSelection(
		share, root, rootGeneration,
		[]transfer.OutputSelectionDirectory{directory}, []transfer.OutputSelectionFile{file},
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

type v3SemanticEntryRef struct {
	kind     outputV3EntryKind
	closeErr error
	closes   atomic.Int64
}

type v3SemanticCountingDirectory struct {
	outputV3Directory
	closes atomic.Int64
}

func (directory *v3SemanticCountingDirectory) Close() error {
	directory.closes.Add(1)
	return directory.outputV3Directory.Close()
}

func (entry *v3SemanticEntryRef) Kind() outputV3EntryKind  { return entry.kind }
func (*v3SemanticEntryRef) AllocatedSize() (uint64, error) { return 0, nil }
func (entry *v3SemanticEntryRef) Close() error {
	entry.closes.Add(1)
	return entry.closeErr
}

func TestOutputV3AuthorityInventoryRejectsForgedReferences(t *testing.T) {
	authority := ResumeStateRef{
		rootPath: "root", kind: ResumeStateLegacyUntrusted, legacyName: ".wsresume-output-state",
	}
	inventory := newResumeStateInventory([]ResumeStateSummary{{Reference: authority}})
	defer func() {
		if err := inventory.Close(); err != nil {
			t.Errorf("close inventory: %v", err)
		}
	}()
	reference := inventory.Summaries()[0].Reference

	for name, mutate := range map[string]func(*ResumeStateRef){
		"zero item":     func(candidate *ResumeStateRef) { candidate.itemID = 0 },
		"wrong intent":  func(candidate *ResumeStateRef) { candidate.intent[0]++ },
		"wrong session": func(candidate *ResumeStateRef) { candidate.session[0]++ },
		"wrong kind":    func(candidate *ResumeStateRef) { candidate.kind = ResumeStateRecoverable },
		"wrong legacy":  func(candidate *ResumeStateRef) { candidate.legacyName += ".forged" },
		"wrong owner":   func(candidate *ResumeStateRef) { candidate.inventory = &ResumeStateInventory{} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := reference
			mutate(&candidate)
			if _, err := inventory.consume(candidate); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
				t.Fatalf("forged reference consume = %v, want invalid binding", err)
			}
		})
	}
	consumed, err := inventory.consume(reference)
	if err != nil || consumed.legacyName != authority.legacyName {
		t.Fatalf("consume authentic reference = (%+v, %v)", consumed, err)
	}
	if _, err := inventory.consume(reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("consume authentic reference twice = %v, want invalid binding", err)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.consume(reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("consume through closed inventory = %v, want invalid binding", err)
	}
}

func TestOutputV3OpaqueInventoryRemovesOnlyPinnedSessionEntry(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	incumbentSelection := v3RecoverySelection(t, false, 0)
	incumbent := v3RecoveryOpen(t, authority, root, incumbentSelection)
	v3RecoveryCloseSession(t, incumbent.Session)

	malformedSelection := v3RecoverySelection(t, true, 1)
	emptySelection := v3RecoverySelectionPaths(t, []string{"other.bin"}, 1)
	if malformedSelection.ResumeIntent() == incumbentSelection.ResumeIntent() ||
		emptySelection.ResumeIntent() == incumbentSelection.ResumeIntent() ||
		emptySelection.ResumeIntent() == malformedSelection.ResumeIntent() {
		t.Fatal("opaque-inventory fixtures did not produce distinct resume intents")
	}
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := openInstalledControl(platform.Root(), platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	coordinator, created, err := control.directory.AcquireLock(resumestate.CoordinatorLockName, true)
	if err != nil || created {
		_ = control.Close()
		_ = platform.Close()
		t.Fatalf("acquire fixture coordinator = created %t, err %v", created, err)
	}
	malformedIntentName := resumestate.ResumeNamespaceName(malformedSelection.ResumeIntent())
	malformedIntent, err := control.sessions.CreateDirectory(malformedIntentName, true)
	if err != nil {
		t.Fatal(err)
	}
	const malformedSessionName = "not-a-session"
	payload := []byte("opaque")
	opaque, err := malformedIntent.CreateFile(malformedSessionName, true, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if written, writeErr := opaque.WriteAt(payload, 0); writeErr != nil || written != len(payload) {
		t.Fatalf("write opaque session entry = (%d, %v)", written, writeErr)
	}
	if err := errors.Join(opaque.Sync(), opaque.Close(), malformedIntent.Sync(), malformedIntent.Close()); err != nil {
		t.Fatal(err)
	}
	emptyIntentName := resumestate.ResumeNamespaceName(emptySelection.ResumeIntent())
	emptyIntent, err := control.sessions.CreateDirectory(emptyIntentName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(emptyIntent.Sync(), emptyIntent.Close(), control.sessions.Sync()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(coordinator.Close(), control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	var malformed, empty *ResumeStateSummary
	summaries := inventory.Summaries()
	for index := range summaries {
		summary := &summaries[index]
		switch summary.Reference.ResumeIntent() {
		case malformedSelection.ResumeIntent():
			malformed = summary
		case emptySelection.ResumeIntent():
			empty = summary
		}
	}
	if malformed == nil || malformed.Reference.Kind() != ResumeStateOpaqueUnsafe ||
		!v3RecoveryHasAttention(*malformed, "malformed-session-namespace") {
		t.Fatalf("malformed pinned session summary = %+v", malformed)
	}
	if empty == nil || empty.Reference.Kind() != ResumeStateOpaqueUnsafe ||
		!v3RecoveryHasAttention(*empty, "empty-resume-namespace") {
		t.Fatalf("empty intent summary = %+v", empty)
	}
	settlement, err := authority.discardResumeState(context.Background(), malformed.Reference)
	if err != nil || settlement.Kind != Discarded {
		t.Fatalf("discard pinned opaque session entry = (%+v, %v)", settlement, err)
	}
	if _, err := authority.discardResumeState(context.Background(), empty.Reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("discard unpinned empty intent = %v, want invalid binding", err)
	}
	if _, err := os.Stat(filepath.Join(
		root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName, malformedIntentName,
	)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed intent shell after exact discard stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(
		root, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName, emptyIntentName,
	)); err != nil {
		t.Fatalf("attention-only empty intent changed by refused discard: %v", err)
	}
}

var _ outputV3EntryRef = (*v3SemanticEntryRef)(nil)
