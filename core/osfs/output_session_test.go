package osfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func outputTestID[T ~[16]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func outputTestConfig(root string) FilesystemOutputSessionConfig {
	var intent OutputResumeIntent
	intent[0] = 3
	return FilesystemOutputSessionConfig{
		RootPath: root, ShareInstance: outputTestID[catalog.ShareInstance](1),
		SessionID: outputTestID[transfer.OutputSessionID](2), ResumeIntent: intent,
	}
}

func outputTestDescriptor(t *testing.T, config FilesystemOutputSessionConfig, fileByte, revisionByte byte, size uint64, modified catalog.ModifiedTime) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(size, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		config.ShareInstance, outputTestID[catalog.FileID](fileByte), outputTestID[content.FileRevision](revisionByte), geometry, modified,
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func outputTestFile(path string, descriptor content.FileRevisionDescriptor) transfer.OutputFile {
	return transfer.OutputFile{Path: path, ExpectedSize: descriptor.ExactSize(), Descriptor: descriptor}
}

func crashOutputSession(session *FilesystemOutputSession, transactions ...transfer.FileTransaction) {
	for _, transaction := range transactions {
		if concrete, ok := transaction.(*filesystemFileTransaction); ok {
			concrete.mu.Lock()
			if concrete.file != nil {
				_ = concrete.file.Close()
				concrete.file = nil
			}
			concrete.closed = true
			concrete.mu.Unlock()
		}
	}
	session.mu.Lock()
	session.closed = true
	_ = session.lock.close(false)
	_ = session.root.Close()
	session.mu.Unlock()
}

func TestFilesystemOutputSessionSparseRecoveryPublishEmptyEntriesAndMTime(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if session.ResumeIntent() != config.ResumeIntent {
		t.Fatal("output session changed its stable resume intent")
	}
	if session.Capabilities().Durability < transfer.DurabilityProcessRestart || !session.Capabilities().RandomWrite ||
		!session.Capabilities().FileFailureIsolation {
		t.Fatalf("capabilities=%+v", session.Capabilities())
	}
	if runtime.GOOS == "windows" && session.Capabilities().Durability != transfer.DurabilityProcessRestart {
		t.Fatalf("Windows output overclaimed unprovable power-loss durability: %+v", session.Capabilities())
	}
	if err := session.EnsureDirectory(context.Background(), transfer.OutputDirectory{Path: "folder/empty", ModifiedTime: modified}); err != nil {
		t.Fatal(err)
	}
	size := uint64(catalog.MinChunkSize)*2 + 7
	descriptor := outputTestDescriptor(t, config, 3, 4, size, modified)
	transaction, initial, err := session.BeginFile(context.Background(), outputTestFile("folder/data.bin", descriptor))
	if err != nil || !initial.Ranges().IsEmpty() {
		t.Fatalf("initial=%v err=%v", initial.Ranges().Ranges(), err)
	}
	second := bytes.Repeat([]byte{0x22}, catalog.MinChunkSize)
	if err := transaction.WriteRange(context.Background(), uint64(catalog.MinChunkSize), second); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil || !slices.Equal(checkpoint.Ranges().Ranges(), []content.Range{{Offset: uint64(catalog.MinChunkSize), End: uint64(catalog.MinChunkSize) * 2}}) {
		t.Fatalf("checkpoint=%v err=%v", checkpoint.Ranges().Ranges(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "folder", "data.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted final file exists: %v", err)
	}
	crashOutputSession(session, transaction)

	resumed, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	transaction, durable, err := resumed.BeginFile(context.Background(), outputTestFile("folder/data.bin", descriptor))
	if err != nil || durable.Binding() != transaction.Binding() || !slices.Equal(durable.Ranges().Ranges(), checkpoint.Ranges().Ranges()) {
		t.Fatalf("durable=%v err=%v", durable.Ranges().Ranges(), err)
	}
	first := bytes.Repeat([]byte{0x11}, catalog.MinChunkSize)
	tail := bytes.Repeat([]byte{0x33}, 7)
	if err := transaction.WriteRange(context.Background(), 0, first); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), uint64(catalog.MinChunkSize)*2, tail); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	emptyDescriptor := outputTestDescriptor(t, config, 5, 6, 0, modified)
	emptyTransaction, emptyDurable, err := resumed.BeginFile(context.Background(), outputTestFile("folder/empty.txt", emptyDescriptor))
	if err != nil || !emptyDurable.Ranges().IsEmpty() {
		t.Fatalf("empty begin durable=%v err=%v", emptyDurable.Ranges().Ranges(), err)
	}
	if err := emptyTransaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := resumed.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "folder/empty", ModifiedTime: modified}); err != nil {
		t.Fatal(err)
	}
	if err := resumed.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "folder", ModifiedTime: modified}); err != nil {
		t.Fatal(err)
	}
	if err := resumed.FinishJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "folder", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(first, second...), tail...)
	if !bytes.Equal(got, want) {
		t.Fatal("published sparse file bytes changed")
	}
	if info, err := os.Stat(filepath.Join(root, "folder", "empty.txt")); err != nil || info.Size() != 0 {
		t.Fatalf("empty file info=%v err=%v", info, err)
	}
	if info, err := os.Stat(filepath.Join(root, "folder", "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory info=%v err=%v", info, err)
	}
	if session.Capabilities().ModifiedTime {
		info, err := os.Stat(filepath.Join(root, "folder", "data.bin"))
		if err != nil || info.ModTime().Unix() != modified.Seconds() {
			t.Fatalf("file mtime=%v err=%v", info.ModTime(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, resumed.journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed journal still exists: %v", err)
	}
}

func TestFilesystemCheckpointOrderAndCrashCuts(t *testing.T) {
	orderedRoot := t.TempDir()
	orderedConfig := outputTestConfig(orderedRoot)
	var ordered []checkpointPhase
	orderedSession, err := newFilesystemOutputSession(orderedConfig, func(phase checkpointPhase) error {
		ordered = append(ordered, phase)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := outputTestDescriptor(t, orderedConfig, 10, 11, 64, catalog.ModifiedTime{})
	transaction, _, err := orderedSession.BeginFile(context.Background(), outputTestFile("ordered.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	ordered = nil
	if err := transaction.WriteRange(context.Background(), 0, bytes.Repeat([]byte{1}, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantOrder := []checkpointPhase{
		checkpointDataWrite, checkpointDataFlush, checkpointJournalWrite, checkpointJournalFlush,
		checkpointJournalInstall, checkpointReopenVerify,
	}
	if !slices.Equal(ordered, wantOrder) {
		t.Fatalf("checkpoint order=%v want=%v", ordered, wantOrder)
	}
	_, _ = transaction.Abort(context.Background(), errors.New("test cleanup"))
	_ = orderedSession.AbortJob(context.Background(), errors.New("test cleanup"))

	for _, cut := range wantOrder {
		t.Run(phaseName(cut), func(t *testing.T) {
			root := t.TempDir()
			config := outputTestConfig(root)
			armed := false
			crashErr := errors.New("simulated crash")
			var observed []checkpointPhase
			session, err := newFilesystemOutputSession(config, func(phase checkpointPhase) error {
				if !armed {
					return nil
				}
				observed = append(observed, phase)
				if phase == cut {
					return crashErr
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor := outputTestDescriptor(t, config, 12, 13, 64, catalog.ModifiedTime{})
			transaction, _, err := session.BeginFile(context.Background(), outputTestFile("cut.bin", descriptor))
			if err != nil {
				t.Fatal(err)
			}
			armed = true
			writeErr := transaction.WriteRange(context.Background(), 0, bytes.Repeat([]byte{9}, 32))
			if cut != checkpointDataWrite && writeErr == nil {
				_, writeErr = transaction.Checkpoint(context.Background())
			}
			if !errors.Is(writeErr, crashErr) {
				t.Fatalf("cut error=%v", writeErr)
			}
			cutIndex := slices.Index(wantOrder, cut)
			if !slices.Equal(observed, wantOrder[:cutIndex+1]) {
				t.Fatalf("observed=%v want=%v", observed, wantOrder[:cutIndex+1])
			}
			crashOutputSession(session, transaction)
			resumed, err := NewFilesystemOutputSession(config)
			if err != nil {
				t.Fatal(err)
			}
			resumedTransaction, durable, err := resumed.BeginFile(context.Background(), outputTestFile("cut.bin", descriptor))
			if err != nil {
				t.Fatal(err)
			}
			wantDurable := cut >= checkpointJournalInstall
			if got := !durable.Ranges().IsEmpty(); got != wantDurable {
				t.Fatalf("cut=%v durable=%v wantPublishedAfterRecovery=%v", cut, durable.Ranges().Ranges(), wantDurable)
			}
			_, _ = resumedTransaction.Abort(context.Background(), errors.New("test cleanup"))
			_ = resumed.AbortJob(context.Background(), errors.New("test cleanup"))
		})
	}
}

func phaseName(phase checkpointPhase) string {
	return map[checkpointPhase]string{
		checkpointDataWrite: "data-write", checkpointDataFlush: "data-flush",
		checkpointJournalWrite: "journal-write", checkpointJournalFlush: "journal-flush",
		checkpointJournalInstall: "journal-install", checkpointReopenVerify: "reopen-verify",
	}[phase]
}

func TestFilesystemOutputBindingChangeAndObjectReplacementInvalidateOnlyOneFile(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	descriptorA := outputTestDescriptor(t, config, 20, 21, 64, catalog.ModifiedTime{})
	descriptorB := outputTestDescriptor(t, config, 22, 23, 64, catalog.ModifiedTime{})
	transactionA, _, _ := session.BeginFile(context.Background(), outputTestFile("a.bin", descriptorA))
	transactionB, _, _ := session.BeginFile(context.Background(), outputTestFile("b.bin", descriptorB))
	for _, transaction := range []transfer.FileTransaction{transactionA, transactionB} {
		if err := transaction.WriteRange(context.Background(), 0, bytes.Repeat([]byte{7}, 32)); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	entryB, _ := session.journal.file("b.bin")
	stageB := entryB.Stage
	crashOutputSession(session, transactionA, transactionB)
	if err := os.Remove(filepath.Join(root, stageB)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stageB), make([]byte, 64), filePerm); err != nil {
		t.Fatal(err)
	}

	resumed, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	changedA := outputTestDescriptor(t, config, 20, 24, 64, catalog.ModifiedTime{})
	newA, durableA, err := resumed.BeginFile(context.Background(), outputTestFile("a.bin", changedA))
	if err != nil || !durableA.Ranges().IsEmpty() {
		t.Fatalf("changed revision durable=%v err=%v", durableA.Ranges().Ranges(), err)
	}
	newB, durableB, err := resumed.BeginFile(context.Background(), outputTestFile("b.bin", descriptorB))
	if err != nil || !durableB.Ranges().IsEmpty() {
		t.Fatalf("replaced object durable=%v err=%v", durableB.Ranges().Ranges(), err)
	}
	if durableA.Binding().FileRevision() == descriptorA.FileRevision() || durableB.Binding().ObjectIdentity() == transactionB.Binding().ObjectIdentity() {
		t.Fatal("invalidated file retained an old binding")
	}
	_, _ = newA.Abort(context.Background(), errors.New("test cleanup"))
	_, _ = newB.Abort(context.Background(), errors.New("test cleanup"))
	_ = resumed.AbortJob(context.Background(), errors.New("test cleanup"))
}

func TestFilesystemOutputRejectsOverwriteEscapeAndJournalBindingTamper(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	if err := os.WriteFile(filepath.Join(root, "existing.bin"), []byte("keep"), filePerm); err != nil {
		t.Fatal(err)
	}
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := outputTestDescriptor(t, config, 30, 31, 4, catalog.ModifiedTime{})
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("existing.bin", descriptor)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("overwrite error=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "existing.bin")); string(got) != "keep" {
		t.Fatalf("existing file changed to %q", got)
	}
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("../escape.bin", descriptor)); err == nil {
		t.Fatal("escaping output path accepted")
	}
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("owned.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	crashOutputSession(session, transaction)
	journalRoot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journalRoot.Close() })
	document, err := readOutputJournalAt(journalRoot, session.journalName)
	if err != nil {
		t.Fatal(err)
	}
	document.ShareInstance = encodeOutputBytes(outputTestID[catalog.ShareInstance](99).Bytes())
	if _, err := persistOutputJournal(journalRoot, session.journalName, document, nil); err != nil {
		t.Fatal(err)
	}
	_ = journalRoot.Close()
	if _, err := NewFilesystemOutputSession(config); !errors.Is(err, ErrOutputBinding) {
		t.Fatalf("tampered source binding error=%v", err)
	}

	encoded, err := encodeOutputJournal(document)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if _, err := decodeOutputJournal(encoded); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("checksum error=%v", err)
	}
}

func TestFilesystemOutputRejectsJournalTransplantedToAnotherRoot(t *testing.T) {
	parent := t.TempDir()
	sourceRoot := filepath.Join(parent, "source")
	targetRoot := filepath.Join(parent, "target")
	if err := os.MkdirAll(sourceRoot, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetRoot, dirPerm); err != nil {
		t.Fatal(err)
	}
	sourceConfig := outputTestConfig(sourceRoot)
	session, err := NewFilesystemOutputSession(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := outputTestDescriptor(t, sourceConfig, 34, 35, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, _ := session.journal.file("file.bin")
	crashOutputSession(session, transaction)
	if err := os.Link(filepath.Join(sourceRoot, entry.Stage), filepath.Join(targetRoot, entry.Stage)); err != nil {
		t.Skipf("filesystem does not support the hard-link transplant fixture: %v", err)
	}
	journalBytes, err := os.ReadFile(filepath.Join(sourceRoot, session.journalName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, session.journalName), journalBytes, filePerm); err != nil {
		t.Fatal(err)
	}
	targetConfig := sourceConfig
	targetConfig.RootPath = targetRoot
	if _, err := NewFilesystemOutputSession(targetConfig); !errors.Is(err, ErrOutputBinding) {
		t.Fatalf("transplanted root binding error=%v", err)
	}
}

func TestFilesystemOutputRetainsAuthorizedRootAcrossPathReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "authorized")
	replacement := filepath.Join(parent, "replacement")
	if err := os.Mkdir(root, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, dirPerm); err != nil {
		t.Fatal(err)
	}
	config := outputTestConfig(root)
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	// Model the textual path resolving to another object after authorization.
	// Every mutation must continue through the retained root, never rootPath.
	session.rootPath = replacement
	descriptor := outputTestDescriptor(t, config, 44, 45, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("retained.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, session.journalName)); err != nil {
		t.Fatalf("journal left the retained root: %v", err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "retained.bin")); err != nil || string(got) != "data" {
		t.Fatalf("retained-root output=%q err=%v", got, err)
	}
	if entries, err := os.ReadDir(replacement); err != nil || len(entries) != 0 {
		t.Fatalf("replacement root was mutated: entries=%v err=%v", entries, err)
	}
	if err := session.FinishJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputAbortIsolationAndConcurrentTransactions(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	const files = 8
	var wait sync.WaitGroup
	errorsOut := make(chan error, files)
	for index := range files {
		wait.Go(func() {
			descriptor := outputTestDescriptor(t, config, byte(40+index), byte(60+index), 128, catalog.ModifiedTime{})
			path := filepath.ToSlash(filepath.Join("parallel", phaseName(checkpointPhase(index+1))+"-"+string(rune('a'+index))+".bin"))
			transaction, _, err := session.BeginFile(context.Background(), outputTestFile(path, descriptor))
			if err != nil {
				errorsOut <- err
				return
			}
			if index == 0 {
				_, err = transaction.Abort(context.Background(), errors.New("isolated file failure"))
				errorsOut <- err
				return
			}
			if err = transaction.WriteRange(context.Background(), 0, bytes.Repeat([]byte{byte(index)}, 128)); err == nil {
				_, err = transaction.Checkpoint(context.Background())
			}
			if err == nil {
				err = transaction.Commit(context.Background())
			}
			errorsOut <- err
		})
	}
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := session.FinishJob(context.Background(), transfer.JobCompletedWithErrors); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "parallel"))
	if err != nil || len(entries) != files-1 {
		t.Fatalf("published entries=%d err=%v", len(entries), err)
	}
}

func TestFilesystemOutputSessionIdentityHasExclusiveWriterAuthority(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	first, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystemOutputSession(config); !errors.Is(err, ErrOutputSessionActive) {
		t.Fatalf("concurrent session authority error=%v", err)
	}
	if err := first.AbortJob(context.Background(), errors.New("release authority")); err != nil {
		t.Fatal(err)
	}
	resumed, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatalf("released session authority was not reusable: %v", err)
	}
	if err := resumed.AbortJob(context.Background(), errors.New("cleanup")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputAbortJobPreservesPublishedFilesAndCleansStaging(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	committedDescriptor := outputTestDescriptor(t, config, 80, 81, 4, catalog.ModifiedTime{})
	committed, _, _ := session.BeginFile(context.Background(), outputTestFile("folder/committed.bin", committedDescriptor))
	_ = committed.WriteRange(context.Background(), 0, []byte("done"))
	_, _ = committed.Checkpoint(context.Background())
	if err := committed.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	partialDescriptor := outputTestDescriptor(t, config, 82, 83, 4, catalog.ModifiedTime{})
	partial, _, _ := session.BeginFile(context.Background(), outputTestFile("folder/partial.bin", partialDescriptor))
	_ = partial.WriteRange(context.Background(), 0, []byte("part"))
	if err := session.AbortJob(context.Background(), errors.New("session terminal")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "folder", "committed.bin")); err != nil || string(got) != "done" {
		t.Fatalf("committed output=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "folder", "partial.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial final exists: %v", err)
	}
	stages, _ := filepath.Glob(filepath.Join(root, outputStagePrefix+"*"))
	if len(stages) != 0 {
		t.Fatalf("staging leaked: %v", stages)
	}
}

func TestFilesystemOutputPublicationRacePreservesForeignFile(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 86, 87, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("collision.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("ours")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "collision.bin"), []byte("foreign"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background()); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("publication collision error=%v", err)
	}
	if _, err := transaction.Abort(context.Background(), errors.New("collision cleanup")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "collision.bin")); err != nil || string(got) != "foreign" {
		t.Fatalf("foreign collision output=%q err=%v", got, err)
	}
	if err := session.AbortJob(context.Background(), errors.New("collision cleanup")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputAbortRemovesPublishCutOwnedFile(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 84, 85, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("publish-cut.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	concrete := transaction.(*filesystemFileTransaction)
	concrete.mu.Lock()
	if err := concrete.file.Close(); err != nil {
		concrete.mu.Unlock()
		t.Fatal(err)
	}
	concrete.file = nil
	concrete.mu.Unlock()
	if err := publishOutputFile(session.root, concrete.stage, "publish-cut.bin"); err != nil {
		t.Fatal(err)
	}
	if err := session.AbortJob(context.Background(), errors.New("abort publish cut")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "publish-cut.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted publish-cut output survived abort: %v", err)
	}
}

func TestFilesystemOutputBoundsActiveTransactions(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	for index := range MaxFilesystemOutputTransactions {
		descriptor := outputTestDescriptor(t, config, byte(130+index), byte(170+index), 0, catalog.ModifiedTime{})
		_, _, err := session.BeginFile(context.Background(), outputTestFile(fmt.Sprintf("bounded/%02d.bin", index), descriptor))
		if err != nil {
			t.Fatalf("begin transaction %d: %v", index, err)
		}
	}
	overflow := outputTestDescriptor(t, config, 250, 251, 0, catalog.ModifiedTime{})
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("bounded/overflow.bin", overflow)); !errors.Is(err, ErrOutputTransactionLimit) {
		t.Fatalf("transaction limit error=%v", err)
	}
	if err := session.AbortJob(context.Background(), errors.New("limit test cleanup")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputAbortClosesAdmissionBeforeUnwindingFiles(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 88, 89, 0, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("active.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	concrete := transaction.(*filesystemFileTransaction)
	concrete.mu.Lock()
	abortDone := make(chan error, 1)
	go func() {
		abortDone <- session.AbortJob(context.Background(), errors.New("terminal"))
	}()
	deadline := time.Now().Add(time.Second)
	for {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			concrete.mu.Unlock()
			t.Fatal("abort did not close transaction admission")
		}
		runtime.Gosched()
	}
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("late.bin", descriptor)); !errors.Is(err, ErrOutputSessionClosed) {
		concrete.mu.Unlock()
		t.Fatalf("late transaction error=%v", err)
	}
	concrete.mu.Unlock()
	if err := <-abortDone; err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputConstructorAndTransactionValidation(t *testing.T) {
	if _, err := NewFilesystemOutputSession(FilesystemOutputSessionConfig{}); err == nil {
		t.Fatal("zero output configuration accepted")
	}
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 90, 91, 8, catalog.ModifiedTime{})
	wrongSize := outputTestFile("file.bin", descriptor)
	wrongSize.ExpectedSize++
	if _, _, err := session.BeginFile(context.Background(), wrongSize); err == nil {
		t.Fatal("catalog size mismatch accepted")
	}
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 8, []byte{1}); err == nil {
		t.Fatal("out-of-range write accepted")
	}
	if err := transaction.Commit(context.Background()); !errors.Is(err, transfer.ErrIncompleteOutputFile) {
		t.Fatalf("incomplete commit error=%v", err)
	}
	_, _ = transaction.Abort(context.Background(), errors.New("cleanup"))
	unicodeTransaction, _, err := session.BeginFile(context.Background(), outputTestFile("e\u0301.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("é.bin", descriptor)); !errors.Is(err, ErrOutputFileActive) {
		t.Fatalf("canonically equivalent active path error=%v", err)
	}
	_, _ = unicodeTransaction.Abort(context.Background(), errors.New("cleanup"))
	_ = session.AbortJob(context.Background(), errors.New("cleanup"))
}

func TestFilesystemOutputCancellationAndPathBounds(t *testing.T) {
	longConfig := outputTestConfig(filepath.Join(t.TempDir(), strings.Repeat("x", 5000)))
	if _, err := NewFilesystemOutputSession(longConfig); !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("long output root error=%v", err)
	}
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.EnsureDirectory(canceled, transfer.OutputDirectory{Path: "folder"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory creation error=%v", err)
	}
	if err := session.FinalizeDirectory(canceled, transfer.OutputDirectory{Path: "folder"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled directory finalization error=%v", err)
	}
	if err := session.FinishJob(canceled, transfer.JobSucceeded); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled job finish error=%v", err)
	}
	if err := session.AbortJob(canceled, context.Canceled); err != nil {
		t.Fatalf("abort did not clean up independently of cancellation: %v", err)
	}
}

func TestFilesystemOutputHandleFailuresRemainOwnedAndAbortable(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		name := "write"
		if checkpoint {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			config := outputTestConfig(root)
			session, _ := NewFilesystemOutputSession(config)
			idOffset := byte(0)
			if checkpoint {
				idOffset = 1
			}
			descriptor := outputTestDescriptor(t, config, 92+idOffset, 94+idOffset, 4, catalog.ModifiedTime{})
			transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
			if err != nil {
				t.Fatal(err)
			}
			concrete := transaction.(*filesystemFileTransaction)
			if checkpoint {
				if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
					t.Fatal(err)
				}
			}
			if err := concrete.file.Close(); err != nil {
				t.Fatal(err)
			}
			if checkpoint {
				_, err = transaction.Checkpoint(context.Background())
			} else {
				err = transaction.WriteRange(context.Background(), 0, []byte("data"))
			}
			if err == nil {
				t.Fatal("closed output handle accepted I/O")
			}
			if _, err := transaction.Abort(context.Background(), errors.New("handle invalidated")); err == nil {
				t.Fatal("closed-handle error was lost during abort")
			}
			if err := session.AbortJob(context.Background(), errors.New("cleanup")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCatalogTimePreservesAuthorizedPrecision(t *testing.T) {
	modified, err := catalog.NewModifiedTime(123, 456_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogTime(modified); !got.Equal(time.Unix(123, 456_000_000)) {
		t.Fatalf("catalog time=%v", got)
	}
}

func TestFilesystemOutputDefensiveStateTransitions(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if session.BackendID() != filesystemOutputBackendID || session.SessionID() != config.SessionID {
		t.Fatal("output identity getters changed")
	}
	if err := session.EnsureDirectory(context.Background(), transfer.OutputDirectory{Path: "existing"}); err != nil {
		t.Fatal(err)
	}
	if err := session.EnsureDirectory(context.Background(), transfer.OutputDirectory{Path: "existing"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-directory"), []byte("foreign"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := session.EnsureDirectory(context.Background(), transfer.OutputDirectory{Path: "not-directory/child"}); err == nil {
		t.Fatal("ordinary file accepted as an output directory")
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "existing"}); err != nil {
		t.Fatal(err)
	}
	modified, _ := catalog.NewModifiedTime(100, 0, catalog.TimePrecisionSeconds)
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "not-directory", ModifiedTime: modified}); err == nil {
		t.Fatal("ordinary file finalized as a directory")
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "missing", ModifiedTime: modified}); err == nil {
		t.Fatal("missing directory finalized")
	}
	descriptor := outputTestDescriptor(t, config, 100, 101, 16, catalog.ModifiedTime{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := session.BeginFile(cancelled, outputTestFile("cancelled.bin", descriptor)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled begin error=%v", err)
	}
	transaction, initial, err := session.BeginFile(context.Background(), outputTestFile("active.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("active.bin", descriptor)); !errors.Is(err, ErrOutputFileActive) {
		t.Fatalf("duplicate active error=%v", err)
	}
	if checkpoint, err := transaction.Checkpoint(context.Background()); err != nil || checkpoint.Generation() != initial.Generation() {
		t.Fatalf("empty checkpoint=%+v err=%v", checkpoint, err)
	}
	if _, err := transaction.Checkpoint(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled checkpoint error=%v", err)
	}
	if err := transaction.WriteRange(context.Background(), 0, nil); err == nil {
		t.Fatal("empty write accepted")
	}
	if err := session.FinishJob(context.Background(), transfer.JobSucceeded); err == nil {
		t.Fatal("job finished with active file")
	}
	if err := session.FinishJob(context.Background(), transfer.JobAborted); err == nil {
		t.Fatal("aborted outcome passed FinishJob")
	}
	if _, err := transaction.Abort(context.Background(), errors.New("cancel file")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Abort(context.Background(), errors.New("repeat")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte{1}); err == nil {
		t.Fatal("closed transaction accepted a write")
	}
	if _, err := transaction.Checkpoint(context.Background()); err == nil {
		t.Fatal("closed transaction checkpointed")
	}
	if err := transaction.Commit(context.Background()); err == nil {
		t.Fatal("closed transaction committed")
	}
	if err := session.AbortJob(context.Background(), errors.New("close")); err != nil {
		t.Fatal(err)
	}
	if err := session.EnsureDirectory(context.Background(), transfer.OutputDirectory{Path: "after-close"}); err == nil {
		t.Fatal("closed session created a directory")
	}
	if err := session.FinalizeDirectory(context.Background(), transfer.OutputDirectory{Path: "existing"}); err == nil {
		t.Fatal("closed session accepted a directory finalization")
	}
	if _, _, err := session.BeginFile(context.Background(), outputTestFile("after-close.bin", descriptor)); err == nil {
		t.Fatal("closed session began a file")
	}
	if err := session.AbortJob(context.Background(), errors.New("repeat")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputRecoversPublishCutAndIdempotentPublishedFiles(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	modified, err := catalog.NewModifiedTime(1_700_000_123, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	descriptorA := outputTestDescriptor(t, config, 110, 111, 4, modified)
	descriptorB := outputTestDescriptor(t, config, 112, 113, 4, catalog.ModifiedTime{})
	transactionA, _, _ := session.BeginFile(context.Background(), outputTestFile("a.bin", descriptorA))
	transactionB, _, _ := session.BeginFile(context.Background(), outputTestFile("b.bin", descriptorB))
	for _, transaction := range []transfer.FileTransaction{transactionA, transactionB} {
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	concreteA := transactionA.(*filesystemFileTransaction)
	concreteA.mu.Lock()
	if err := concreteA.file.Close(); err != nil {
		t.Fatal(err)
	}
	concreteA.file = nil
	concreteA.mu.Unlock()
	if err := publishOutputFile(session.root, concreteA.stage, "a.bin"); err != nil {
		t.Fatal(err)
	}
	if err := transactionB.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	crashOutputSession(session, transactionA)

	resumed, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	resumedA, durableA, err := resumed.BeginFile(context.Background(), outputTestFile("a.bin", descriptorA))
	if err != nil || !transfer.RangesCoverFile(4, durableA.Ranges()) {
		t.Fatalf("publish-cut durable=%v err=%v", durableA.Ranges().Ranges(), err)
	}
	resumedB, durableB, err := resumed.BeginFile(context.Background(), outputTestFile("b.bin", descriptorB))
	if err != nil || !transfer.RangesCoverFile(4, durableB.Ranges()) {
		t.Fatalf("published durable=%v err=%v", durableB.Ranges().Ranges(), err)
	}
	if err := resumedA.WriteRange(context.Background(), 0, []byte("x")); err == nil {
		t.Fatal("published transaction accepted a rewrite")
	}
	if err := resumedA.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if disposition, err := resumedB.Abort(context.Background(), errors.New("already published")); err != nil || disposition != transfer.FileAbortIsolated {
		t.Fatalf("published abort=(%v,%v)", disposition, err)
	}
	if err := resumed.FinishJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a.bin", "b.bin"} {
		if got, err := os.ReadFile(filepath.Join(root, path)); err != nil || string(got) != "data" {
			t.Fatalf("%s=%q err=%v", path, got, err)
		}
	}
	if session.Capabilities().ModifiedTime {
		info, err := os.Stat(filepath.Join(root, "a.bin"))
		if err != nil || info.ModTime().Unix() != modified.Seconds() {
			t.Fatalf("publish-cut mtime=%v err=%v", info.ModTime(), err)
		}
	}
}

func TestFilesystemOutputRevalidatesPublishedIdentityAtCommit(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 122, 123, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	concrete := resumed.(*filesystemFileTransaction)
	if err := concrete.file.Close(); err != nil {
		t.Fatal(err)
	}
	concrete.file = nil
	if err := os.Rename(filepath.Join(root, "file.bin"), filepath.Join(root, "displaced-owned.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.bin"), []byte("evil"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Commit(context.Background()); !errors.Is(err, ErrOwnedFileMissing) {
		t.Fatalf("replaced published identity error=%v", err)
	}
	if _, err := resumed.Abort(context.Background(), errors.New("identity changed")); err != nil {
		t.Fatal(err)
	}
	if err := session.AbortJob(context.Background(), errors.New("identity changed")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "file.bin")); err != nil || string(got) != "evil" {
		t.Fatalf("foreign replacement changed=%q err=%v", got, err)
	}
}

func TestFilesystemOutputRecoveryRemovesPublishCutStageAlias(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 124, 125, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	concrete := transaction.(*filesystemFileTransaction)
	concrete.mu.Lock()
	if err := concrete.file.Close(); err != nil {
		concrete.mu.Unlock()
		t.Fatal(err)
	}
	concrete.file = nil
	concrete.mu.Unlock()
	stagePath := filepath.Join(root, concrete.stage)
	if err := os.Link(stagePath, filepath.Join(root, "file.bin")); err != nil {
		t.Skipf("filesystem does not support a hard-link publish cut: %v", err)
	}
	crashOutputSession(session, transaction)
	resumedSession, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	resumed, durable, err := resumedSession.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil || !transfer.RangesCoverFile(4, durable.Ranges()) {
		t.Fatalf("publish-cut durable=%v err=%v", durable.Ranges().Ranges(), err)
	}
	if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish-cut stage alias survived recovery: %v", err)
	}
	if err := resumed.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := resumedSession.FinishJob(context.Background(), transfer.JobSucceeded); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOutputRejectsPublishedJournalWithoutFinalObject(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 126, 127, 4, catalog.ModifiedTime{})
	transaction, _, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry, _ := session.journal.file("file.bin")
	oldIdentity := transaction.Binding().ObjectIdentity()
	crashOutputSession(session, transaction)
	if err := os.Rename(filepath.Join(root, "file.bin"), filepath.Join(root, entry.Stage)); err != nil {
		t.Fatal(err)
	}

	resumed, err := NewFilesystemOutputSession(config)
	if err != nil {
		t.Fatal(err)
	}
	newTransaction, durable, err := resumed.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil || !durable.Ranges().IsEmpty() || newTransaction.Binding().ObjectIdentity() == oldIdentity {
		t.Fatalf("rolled-back published checkpoint durable=%v binding=%+v err=%v", durable.Ranges().Ranges(), newTransaction.Binding(), err)
	}
	if _, err := os.Stat(filepath.Join(root, entry.Stage)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale published stage survived invalidation: %v", err)
	}
	_, _ = newTransaction.Abort(context.Background(), errors.New("cleanup"))
	_ = resumed.AbortJob(context.Background(), errors.New("cleanup"))
}

func TestFilesystemOutputRejectsCheckpointForForeignStageAndClosedSession(t *testing.T) {
	root := t.TempDir()
	config := outputTestConfig(root)
	session, _ := NewFilesystemOutputSession(config)
	descriptor := outputTestDescriptor(t, config, 120, 121, 4, catalog.ModifiedTime{})
	transaction, durable, err := session.BeginFile(context.Background(), outputTestFile("file.bin", descriptor))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.persistCheckpoint(transaction.Binding(), "foreign-stage", 1, durable.Ranges(), false); err == nil {
		t.Fatal("foreign stage checkpoint accepted")
	}
	if err := session.persistCheckpoint(transaction.Binding(), transaction.(*filesystemFileTransaction).stage, durable.Generation(), durable.Ranges(), false); err == nil {
		t.Fatal("stale generation checkpoint accepted")
	}
	_, _ = transaction.Abort(context.Background(), errors.New("cleanup"))
	_ = session.AbortJob(context.Background(), errors.New("cleanup"))
	if err := session.persistCheckpoint(transaction.Binding(), transaction.(*filesystemFileTransaction).stage, 1, durable.Ranges(), false); err == nil {
		t.Fatal("closed session checkpoint accepted")
	}

	notDirectory := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(notDirectory, []byte("x"), filePerm); err != nil {
		t.Fatal(err)
	}
	badConfig := outputTestConfig(notDirectory)
	if _, err := NewFilesystemOutputSession(badConfig); err == nil {
		t.Fatal("ordinary file accepted as output root")
	}
}
