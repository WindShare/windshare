package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func nativeTestIntent(t *testing.T, root string, shareByte, rootByte byte) transfer.TransferIntent {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		incrementalTestIdentity16[catalog.ShareInstance](shareByte),
		incrementalTestIdentity16[catalog.DirectoryID](rootByte),
		rules, root, filesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func incrementalTestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func TestNativeCompositionOwnsLeaseAndExposesOnlyOutputSession(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0x11, 0x12)
	var traceMu sync.Mutex
	var traces []FilesystemOutputTrace
	newAuthority := func() *Authority {
		authority, err := New(Config{
			RootPath: root, PlatformFactory: openOutputRuntimeTestPlatform,
			Tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
				traceMu.Lock()
				traces = append(traces, event)
				traceMu.Unlock()
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return authority
	}

	first, err := newAuthority().OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.(*outputsession.Session); !ok {
		t.Fatalf("OpenOutput returned %T, want the single outputsession runtime", first)
	}
	if _, err := newAuthority().OpenOutput(context.Background(), intent); err == nil {
		t.Fatal("a concurrent session acquired the same durable intent lease")
	} else {
		normalized := transferfault.NormalizeBoundary(context.Background(), err)
		value, ok := normalized.Fault()
		code, checkpoint := value.CheckpointCode()
		if !ok || !checkpoint || code != transferfault.CheckpointBusy || value.Scope() != transferfault.ScopeOutputPause {
			t.Fatalf("lease contention fault = %v", err)
		}
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopened, err := newAuthority().OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatalf("lease was not released after stable pause: %v", err)
	}
	rootAdmission := admitNativeRoot(t, reopened, intent, 0x13, catalog.ModifiedTime{})
	if _, err := reopened.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := newAuthority().OpenOutput(context.Background(), intent); err == nil {
		t.Fatal("intent lease was released before stable completion")
	}
	if _, err := reopened.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	afterComplete, err := newAuthority().OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatalf("lease was not released after stable completion: %v", err)
	}
	if _, err := afterComplete.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	wantMilestones := map[FilesystemOutputNativeLockMilestone]bool{
		FilesystemOutputNativeLockAcquired:  false,
		FilesystemOutputNativeLockContended: false,
		FilesystemOutputNativeLockReleased:  false,
	}
	for _, event := range traces {
		if event.Operation == TraceNativeLock {
			if _, tracked := wantMilestones[event.NativeLockMilestone]; tracked {
				wantMilestones[event.NativeLockMilestone] = true
			}
		}
	}
	for milestone, found := range wantMilestones {
		if !found {
			t.Errorf("missing native lease milestone %d", milestone)
		}
	}
}

func TestNativeCompositionRejectsStreamBeforeOpeningRootAuthority(t *testing.T) {
	root := t.TempDir()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		incrementalTestIdentity16[catalog.ShareInstance](0x14),
		incrementalTestIdentity16[catalog.DirectoryID](0x15),
		rules, root, filesystemOutputBackendID, transfer.OutputSingleFileStream,
	)
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Uint32
	authority, err := New(Config{
		RootPath: root,
		PlatformFactory: func(string, bool) (outputcap.Platform, error) {
			opens.Add(1)
			return nil, errors.New("stream opened native authority")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.OpenOutput(context.Background(), intent); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("stream intent error = %v", err)
	}
	if opens.Load() != 0 {
		t.Fatal("DurabilityNone stream fabricated native root/checkpoint authority")
	}
}

func TestNativeCompositionAdmitsDelayedSiblingAndSettlesDirectoriesByReceipt(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0x21, 0x22)
	session := openNativeCompositionSession(t, root, false, intent, nil)
	rootAdmission := admitNativeRoot(t, session, intent, 0x23, catalog.ModifiedTime{})

	first := transfer.OutputDirectory{
		DirectoryID:     incrementalTestIdentity16[catalog.DirectoryID](0x24),
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0x25),
		ParentAdmission: rootAdmission, Path: "first",
	}
	firstAdmission, err := session.AdmitDirectory(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.FinalizeDirectory(context.Background(), firstAdmission); err != nil || settlement.Kind() != transfer.DirectoryFinalized {
		t.Fatalf("first directory settlement = (%d, %v)", settlement.Kind(), err)
	}

	// A settled child cannot seal its still-open parent. Catalog discovery may
	// therefore admit a delayed sibling without retaining a global selection.
	second := transfer.OutputDirectory{
		DirectoryID:     incrementalTestIdentity16[catalog.DirectoryID](0x26),
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0x27),
		ParentAdmission: rootAdmission, Path: "second",
	}
	secondAdmission, err := session.AdmitDirectory(context.Background(), second)
	if err != nil {
		t.Fatalf("delayed sibling admission: %v", err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), secondAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil || settlement.Kind() != transfer.JobClosed {
		t.Fatalf("directory-only completion = (%d, %v)", settlement.Kind(), err)
	}
	for _, name := range []string{"first", "second"} {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.IsDir() {
			t.Fatalf("materialized sibling %q = (%v, %v)", name, info, err)
		}
	}
}

func TestNativeCompositionResumesCheckpointAndPreservesNoReplace(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0x31, 0x32)
	first := openNativeCompositionSession(t, root, false, intent, nil)
	firstRoot := admitNativeRoot(t, first, intent, 0x33, catalog.ModifiedTime{})
	descriptor := nativeCompositionDescriptor(t, intent, 0x34, 0x35, 8)
	file := nativeCompositionFile(t, first, descriptor, "resume.bin", firstRoot)
	start, err := first.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("initial BeginFile settled immediately: %+v", start)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("left")); err != nil {
		t.Fatal(err)
	}
	if durable, err := transaction.Checkpoint(context.Background()); err != nil ||
		len(durable.Ranges().Ranges()) != 1 || durable.Ranges().Ranges()[0].End != 4 {
		t.Fatalf("initial checkpoint = (%v, %v)", durable.Ranges().Ranges(), err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopened := openNativeCompositionSession(t, root, false, intent, nil)
	reopenedRoot := admitNativeRoot(t, reopened, intent, 0x33, catalog.ModifiedTime{})
	file = nativeCompositionFile(t, reopened, descriptor, "resume.bin", reopenedRoot)
	start, err = reopened.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, ok := start.Transaction()
	if !ok || len(durable.Ranges().Ranges()) != 1 || durable.Ranges().Ranges()[0].End != 4 {
		t.Fatalf("resumed BeginFile = (transaction=%T ranges=%v)", transaction, durable.Ranges().Ranges())
	}
	if err := transaction.WriteRange(context.Background(), 4, []byte("side")); err != nil {
		t.Fatal(err)
	}
	if settlement, err := transaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("resumed commit = (%d, %v)", settlement.Kind(), err)
	}
	if _, err := reopened.FinalizeDirectory(context.Background(), reopenedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "resume.bin")); err != nil || string(content) != "leftside" {
		t.Fatalf("resumed publication = (%q, %v)", content, err)
	}

	if err := os.WriteFile(filepath.Join(root, "collision.bin"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	collisionIntent := nativeTestIntent(t, root, 0x36, 0x37)
	collisionSession := openNativeCompositionSession(t, root, false, collisionIntent, nil)
	collisionRoot := admitNativeRoot(t, collisionSession, collisionIntent, 0x38, catalog.ModifiedTime{})
	collisionDescriptor := nativeCompositionDescriptor(t, collisionIntent, 0x39, 0x3a, 4)
	collisionStart, err := collisionSession.BeginFile(context.Background(), nativeCompositionFile(
		t, collisionSession, collisionDescriptor, "collision.bin", collisionRoot,
	))
	if err != nil {
		t.Fatal(err)
	}
	collision, ok := collisionStart.ImmediateSettlement()
	if !ok || collision.Kind() != transfer.FileCollision {
		t.Fatalf("preexisting final settlement = (%d, %t)", collision.Kind(), ok)
	}
	if _, err := collisionSession.FinalizeDirectory(context.Background(), collisionRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := collisionSession.CompleteJob(context.Background(), transfer.JobCompletedWithErrors); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "collision.bin")); err != nil || string(content) != "keep" {
		t.Fatalf("collision target was replaced = (%q, %v)", content, err)
	}
}

func TestNativeCompositionResumesPublishedFileThroughPreexistingDescendant(t *testing.T) {
	root := t.TempDir()
	intent := nativeTestIntent(t, root, 0x3b, 0x3c)
	modified, err := catalog.NewModifiedTime(1_600_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	directory := transfer.OutputDirectory{
		DirectoryID:  incrementalTestIdentity16[catalog.DirectoryID](0x3d),
		Generation:   incrementalTestIdentity16[catalog.DirectoryGeneration](0x3e),
		ModifiedTime: modified,
		Path:         "nested",
	}
	descriptor := nativeCompositionDescriptor(t, intent, 0x3f, 0x40, 4)

	first := openNativeCompositionSession(t, root, false, intent, nil)
	firstRoot := admitNativeRoot(t, first, intent, 0x41, catalog.ModifiedTime{})
	directory.ParentAdmission = firstRoot
	firstDirectory, err := first.AdmitDirectory(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	start, err := first.BeginFile(context.Background(), nativeCompositionFile(
		t, first, descriptor, "nested/published.bin", firstDirectory,
	))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("initial BeginFile settled immediately: %+v", start)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if settlement, err := transaction.Commit(context.Background()); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("initial publication = (%d, %v)", settlement.Kind(), err)
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	userModified := time.Unix(1_500_000_000, 0)
	nestedPath := filepath.Join(root, "nested")
	if err := os.Chtimes(nestedPath, userModified, userModified); err != nil {
		t.Fatal(err)
	}
	reopened := openNativeCompositionSession(t, root, false, intent, nil)
	reopenedRoot := admitNativeRoot(t, reopened, intent, 0x41, catalog.ModifiedTime{})
	directory.ParentAdmission = reopenedRoot
	reopenedDirectory, err := reopened.AdmitDirectory(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	start, err = reopened.BeginFile(context.Background(), nativeCompositionFile(
		t, reopened, descriptor, "nested/published.bin", reopenedDirectory,
	))
	if err != nil {
		t.Fatal(err)
	}
	settlement, ok := start.ImmediateSettlement()
	if !ok || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("published checkpoint resume = (%d, %t)", settlement.Kind(), ok)
	}
	if _, err := reopened.FinalizeDirectory(context.Background(), reopenedDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.FinalizeDirectory(context.Background(), reopenedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(nestedPath, "published.bin")); err != nil || string(content) != "kept" {
		t.Fatalf("published file after restart = (%q, %v)", content, err)
	}
	if info, err := os.Stat(nestedPath); err != nil || info.ModTime().Unix() != userModified.Unix() {
		t.Fatalf("preexisting directory metadata = (%v, %v)", info, err)
	}
}

func TestNativeCompositionRecoversAuthorityCreatedRootDisposition(t *testing.T) {
	root := filepath.Join(t.TempDir(), "created-output")
	intent := nativeTestIntent(t, root, 0x41, 0x42)
	var mu sync.Mutex
	var openedDispositions []FilesystemOutputRootDisposition
	tracer := FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		if event.Operation != TraceSessionOpened {
			return
		}
		mu.Lock()
		openedDispositions = append(openedDispositions, event.RootOpenDisposition)
		mu.Unlock()
	})
	first := openNativeCompositionSession(t, root, true, intent, tracer)
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	reopened := openNativeCompositionSession(t, root, true, intent, tracer)
	rootAdmission := admitNativeRoot(t, reopened, intent, 0x43, modified)
	if _, err := reopened.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CompleteJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || info.ModTime().Unix() != modified.Seconds() {
		t.Fatalf("authority-created root metadata = (%v, %v)", info, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(openedDispositions) != 2 {
		t.Fatalf("session-open trace count = %d", len(openedDispositions))
	}
	for _, disposition := range openedDispositions {
		if disposition != FilesystemOutputAuthorityCreatedRoot {
			t.Fatalf("persisted root disposition = %q", disposition)
		}
	}
}

func openNativeCompositionSession(
	t *testing.T,
	root string,
	create bool,
	intent transfer.TransferIntent,
	tracer FilesystemOutputTracer,
) transfer.OutputSession {
	t.Helper()
	authority, err := New(Config{
		RootPath: root, CreateRoot: create, PlatformFactory: openOutputRuntimeTestPlatform, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func admitNativeRoot(
	t *testing.T,
	session transfer.OutputSession,
	intent transfer.TransferIntent,
	generation byte,
	modified catalog.ModifiedTime,
) transfer.DirectoryAdmission {
	t.Helper()
	admission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID:  intent.SyntheticRoot(),
		Generation:   incrementalTestIdentity16[catalog.DirectoryGeneration](generation),
		ModifiedTime: modified,
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func nativeCompositionDescriptor(
	t *testing.T,
	intent transfer.TransferIntent,
	fileID byte,
	revision byte,
	exactSize uint64,
) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(exactSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), incrementalTestIdentity16[catalog.FileID](fileID),
		incrementalTestIdentity16[content.FileRevision](revision), geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func nativeCompositionFile(
	t *testing.T,
	session transfer.OutputSession,
	descriptor content.FileRevisionDescriptor,
	path string,
	parent transfer.DirectoryAdmission,
) transfer.OutputFile {
	t.Helper()
	locator, err := transfer.NewPathOutputLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(
		filesystemOutputBackendID, session.SessionID(), descriptor, locator,
	)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{
		Descriptor: descriptor, ExpectedSize: descriptor.ExactSize(),
		ParentAdmission: parent, Path: path, Target: target,
	}
}
