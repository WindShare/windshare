package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestDirectDirectoryCommitIsPageAddressedIdempotentAndConflictSafe(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "session")
	root := idValue[DirectoryID](130)
	firstDirectory := idValue[DirectoryID](131)
	secondDirectory := idValue[DirectoryID](132)
	firstRecord := selectedDirectory(t, root, 131, "first")
	secondRecord := selectedDirectory(t, root, 132, "second")
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](133),
		[]NodeRecord{firstRecord, secondRecord},
	), session)

	alpha, err := scannedFile(t, 134, "alpha", 1).nodeRecord(firstDirectory)
	if err != nil {
		t.Fatal(err)
	}
	zeta, err := scannedFile(t, 135, "zeta", 2).nodeRecord(firstDirectory)
	if err != nil {
		t.Fatal(err)
	}
	commit := DirectoryCommit{
		directory: firstRecord, generation: idValue[DirectoryGeneration](136),
		children: newSliceNodeSource([]NodeRecord{alpha, zeta}), omittedCount: 2,
	}
	if commit.Directory() != firstRecord || commit.Generation() != idValue[DirectoryGeneration](136) ||
		commit.EntryCount() != 2 || commit.OmittedCount() != 2 {
		t.Fatalf("commit accessors = %+v", commit)
	}
	committed, err := store.CommitDirectory(context.Background(), commit, session)
	if err != nil {
		t.Fatal(err)
	}
	if committed.OmittedCount() != 2 || committed.TerminalCommitment().IsZero() ||
		committed.IsZero() || !committed.Equal(committed) {
		t.Fatalf("committed metadata = %+v", committed)
	}
	before := store.shareBudget.Snapshot().Used
	replayed, err := store.CommitDirectory(context.Background(), commit, session)
	if err != nil || replayed != committed || store.shareBudget.Snapshot().Used != before {
		t.Fatalf("idempotent replay = %+v err=%v", replayed, err)
	}
	changed, _ := scannedFile(t, 137, "changed", 3).nodeRecord(firstDirectory)
	conflict := DirectoryCommit{
		directory: firstRecord, generation: commit.generation,
		children: newSliceNodeSource([]NodeRecord{changed}),
	}
	if _, err := store.CommitDirectory(context.Background(), conflict, session); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("same-generation conflict = %v", err)
	}

	empty := DirectoryCommit{
		directory: secondRecord, generation: idValue[DirectoryGeneration](138),
		children: newSliceNodeSource(nil),
	}
	emptyCommitted, err := store.CommitDirectory(context.Background(), empty, session)
	if err != nil || emptyCommitted.EntryCount() != 0 || emptyCommitted.PageCount() != 1 {
		t.Fatalf("empty generation = %+v err=%v", emptyCommitted, err)
	}
	page, ok, err := store.Page(context.Background(), secondDirectory, emptyCommitted.Generation(), 0)
	if err != nil || !ok || !page.Terminal() || len(page.Entries()) != 0 {
		t.Fatalf("empty terminal page = %+v ok=%v err=%v", page, ok, err)
	}
	if _, err := store.CommitDirectory(context.Background(), rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](139), []NodeRecord{firstRecord},
	), session); err == nil {
		t.Fatal("generic commit accepted a synthetic root")
	}
}

func TestCatalogCodecRoundTripsPrivateRecordsAndRejectsCorruption(t *testing.T) {
	modified, _ := NewModifiedTime(10, 123_000_000, TimePrecisionMilliseconds)
	parent := idValue[DirectoryID](140)
	fileChild := scannedFile(t, 141, "file", 42)
	fileChild.ModifiedTime = modified
	fileRecord, _ := fileChild.nodeRecord(parent)
	directoryChild := scannedDirectory(t, 142, "directory")
	directoryChild.ModifiedTime = modified
	directoryRecord, _ := directoryChild.nodeRecord(parent)
	synthetic, _ := NewSyntheticRootNodeRecord(idValue[DirectoryID](143))
	for _, record := range []NodeRecord{fileRecord, directoryRecord, synthetic} {
		encoded, err := encodeNodeRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeNodeRecord(encoded)
		if err != nil || decoded != record {
			t.Fatalf("node round trip = %+v err=%v", decoded, err)
		}
	}
	if _, err := restoreModifiedTime(storedModifiedTime{Seconds: 1}); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("absent modified time with data = %v", err)
	}
	if _, err := restoreModifiedTime(storedModifiedTime{Present: true, Precision: 99}); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("invalid modified precision = %v", err)
	}
	if _, err := restoreEntry(storedEntry{Kind: 99}); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("invalid stored entry = %v", err)
	}

	valid, err := encodeNodeRecord(fileRecord)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]storedNode{
		"schema": {
			Schema: 99,
		},
		"synthetic-parent": {
			Schema: catalogStorageSchema, Kind: uint8(NodeKindDirectory), ID: synthetic.NodeID().Bytes(),
			Parent: idValue[DirectoryID](1).Bytes(), SyntheticRoot: true,
		},
		"directory-file-fields": {
			Schema: catalogStorageSchema, Kind: uint8(NodeKindDirectory), ID: directoryRecord.NodeID().Bytes(),
			Parent: parent.Bytes(), Name: "directory", SourceIdentity: []byte("source"),
			VersionCandidate: []byte("unexpected"), RelativePath: "directory",
		},
		"file-without-candidate": {
			Schema: catalogStorageSchema, Kind: uint8(NodeKindFile), ID: fileRecord.NodeID().Bytes(),
			Parent: parent.Bytes(), Name: "file", SourceIdentity: []byte("source"), RelativePath: "file",
		},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := catalogStorageEnc.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeNodeRecord(encoded); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("corrupt node accepted: %v", err)
			}
		})
	}
	if _, err := decodeNodeRecord(append(valid, 0)); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("non-canonical node accepted: %v", err)
	}

	page, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1), DirectoryID: parent,
		Generation: idValue[DirectoryGeneration](144), Entries: []Entry{fileRecord.Entry()}, Terminal: true,
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	entry, present := page.Entry(0)
	if page.EntryCount() != 1 || !present || entry.NodeID() != fileRecord.Entry().NodeID() {
		t.Fatalf("page entry accessor count=%d present=%v entry=%+v", page.EntryCount(), present, entry)
	}
	if _, present := page.Entry(1); present {
		t.Fatal("page entry accessor accepted an out-of-range index")
	}
	pageBytes, _ := encodeCatalogPage(page)
	replayed, err := decodeCatalogPage(pageBytes)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, _ := encodeCatalogPage(replayed)
	if !bytes.Equal(pageBytes, reencoded) {
		t.Fatal("page codec changed canonical bytes")
	}
	var stored storedPage
	if err := catalogStorageDec.Unmarshal(pageBytes, &stored); err != nil {
		t.Fatal(err)
	}
	stored.PageIndex = 1
	stored.Previous = make([]byte, PageCommitmentBytes)
	malformed, _ := catalogStorageEnc.Marshal(stored)
	if _, err := decodeCatalogPage(malformed); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("malformed page sequence accepted: %v", err)
	}
}

func TestFileSpillLifecycleAndFinishedObjects(t *testing.T) {
	factory := NewFileSpillFactory(t.TempDir())
	share := idValue[ShareInstance](1)
	attempt := idValue[ScanAttemptID](2)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := factory.Recover(cancelled, share); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recovery = %v", err)
	}
	if err := factory.Recover(context.Background(), ShareInstance{}); err == nil {
		t.Fatal("zero-share recovery succeeded")
	}
	if err := factory.Destroy(ShareInstance{}); err == nil {
		t.Fatal("zero-share cleanup succeeded")
	}
	if _, err := factory.NewWorkspace(cancelled, SpillRequest{ShareInstance: share, AttemptID: attempt}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled workspace = %v", err)
	}
	if _, err := factory.NewWorkspace(context.Background(), SpillRequest{}); err == nil {
		t.Fatal("identity-free workspace succeeded")
	}
	if err := factory.Recover(context.Background(), share); err != nil {
		t.Fatal(err)
	}
	workspace, err := factory.NewWorkspace(context.Background(), SpillRequest{ShareInstance: share, AttemptID: attempt})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := workspace.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("aborted")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(nil); err == nil {
		t.Fatal("finished writer accepted data")
	}
	if _, err := writer.Commit(); err == nil {
		t.Fatal("aborted writer committed")
	}

	writer, _ = workspace.Create(context.Background())
	if _, err := writer.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	object, err := writer.Commit()
	if err != nil || object.Size() != 7 {
		t.Fatalf("committed spill object size=%d err=%v", object.Size(), err)
	}
	if _, err := object.Open(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled object open = %v", err)
	}
	reader, err := object.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "payload" {
		t.Fatalf("spill payload = %q err=%v", data, err)
	}
	_ = reader.Close()
	if err := object.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := object.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := object.Open(context.Background()); err == nil {
		t.Fatal("removed object reopened")
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Create(context.Background()); err == nil {
		t.Fatal("closed workspace created a writer")
	}
	if err := factory.Destroy(share); err != nil {
		t.Fatal(err)
	}
}

func TestScanFailureConstructorsAndAttemptAdmissionAxes(t *testing.T) {
	for _, test := range []struct {
		duration time.Duration
		want     time.Duration
	}{
		{0, DefaultScanRetryCooldown},
		{time.Nanosecond, MinScanRetryCooldown},
		{time.Hour, MaxScanRetryCooldown},
	} {
		var classified *ScanError
		err := NewTransientScanError(nil, test.duration)
		if !errors.As(err, &classified) || classified.cooldown != test.want ||
			classified.Error() == "" || classified.Unwrap() == nil {
			t.Fatalf("transient classification = %+v", classified)
		}
	}
	permanent := NewPermanentScanError(nil)
	var classified *ScanError
	if !errors.As(permanent, &classified) || classified.Error() == "" || classified.Unwrap() == nil {
		t.Fatalf("permanent classification = %v", permanent)
	}

	for name, generators := range map[string]struct {
		attemptIDs  ScanAttemptIDGenerator
		generations DirectoryGenerationGenerator
	}{
		"zero-attempt": {
			attemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return ScanAttemptID{}, nil }),
			generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
				return idValue[DirectoryGeneration](1), nil
			}),
		},
		"attempt-error": {
			attemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return ScanAttemptID{}, errors.New("entropy") }),
			generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
				return idValue[DirectoryGeneration](1), nil
			}),
		},
		"zero-generation": {
			attemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return idValue[ScanAttemptID](1), nil }),
			generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
				return DirectoryGeneration{}, nil
			}),
		},
		"generation-error": {
			attemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return idValue[ScanAttemptID](1), nil }),
			generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
				return DirectoryGeneration{}, errors.New("entropy")
			}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			process := generousBudget(t, "process")
			share := generousBudget(t, "share")
			store, err := NewCatalogStore(StoreConfig{
				ShareInstance: idValue[ShareInstance](1), Backend: NewMemoryCatalogBackend(),
				ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{},
				AttemptIDs: generators.attemptIDs, Generations: generators.generations,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session := generousBudget(t, "session")
			root := idValue[DirectoryID](150)
			directory := idValue[DirectoryID](151)
			commitSyntheticRoot(t, store, rootCommit(
				t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](152),
				[]NodeRecord{selectedDirectory(t, root, 151, "directory")},
			), session)
			scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
				t.Fatal("rejected attempt reached scanner")
				return ScanResult{}, nil
			})
			if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner); err == nil {
				t.Fatal("invalid generated identity was accepted")
			}
			if session.Snapshot().Used != (ResourceUsage{}) {
				t.Fatalf("failed admission leaked session budget: %+v", session.Snapshot().Used)
			}
		})
	}
}

func TestAttemptFailureReuseConcurrencyAndWorkAdmission(t *testing.T) {
	t.Run("permanent failure", func(t *testing.T) {
		store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
		defer store.Close()
		session := generousBudget(t, "session")
		directory := prepareScannableDirectory(t, store, session, 160, 162)
		calls := 0
		scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
			calls++
			return ScanResult{}, NewPermanentScanError(nil)
		})
		_, firstErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
		_, secondErr := store.ListChildren(context.Background(), directory, session, ScanOptions{Retry: true}, scanner)
		var first, second *DirectoryFailure
		if !errors.As(firstErr, &first) || !errors.As(secondErr, &second) ||
			first.AttemptID != second.AttemptID || first.Error() == "" || first.Unwrap() == nil || calls != 1 {
			t.Fatalf("permanent failure replay = %v / %v calls=%d", firstErr, secondErr, calls)
		}
	})

	for name, reuseAttempt := range map[string]bool{"attempt": true, "generation": false} {
		t.Run("reject reused "+name, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1, 0)}
			process := generousBudget(t, "process")
			share := generousBudget(t, "share")
			var attempts byte
			var generations byte
			store, err := NewCatalogStore(StoreConfig{
				ShareInstance: idValue[ShareInstance](1), Backend: NewMemoryCatalogBackend(),
				ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{}, Clock: clock,
				AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
					if !reuseAttempt {
						attempts++
					}
					return idValue[ScanAttemptID](attempts + 1), nil
				}),
				Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
					if reuseAttempt {
						generations++
					}
					return idValue[DirectoryGeneration](generations + 1), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session := generousBudget(t, "session")
			directory := prepareScannableDirectory(t, store, session, 165, 166)
			scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
				return ScanResult{}, NewTransientScanError(errors.New("retry"), MinScanRetryCooldown)
			})
			_, firstErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
			clock.Advance(MinScanRetryCooldown)
			if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{Retry: true}, scanner); err == nil {
				t.Fatalf("reused %s identity was accepted", name)
			}
			_, implicitErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
			var first, implicit *DirectoryFailure
			if !errors.As(firstErr, &first) || !errors.As(implicitErr, &implicit) || first.AttemptID != implicit.AttemptID {
				t.Fatal("failed replacement discarded previous attempt authority")
			}
		})
	}

	t.Run("active scan and work", func(t *testing.T) {
		store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
		defer store.Close()
		rootSession := generousBudget(t, "root-session")
		root := idValue[DirectoryID](170)
		firstID := idValue[DirectoryID](171)
		secondID := idValue[DirectoryID](172)
		commitSyntheticRoot(t, store, rootCommit(
			t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](173),
			[]NodeRecord{
				selectedDirectory(t, root, 171, "first"),
				selectedDirectory(t, root, 172, "second"),
			},
		), rootSession)
		session, _ := NewBudgetAccount("limited-session", BudgetLimits{
			ActiveScans: 1, ScanWork: 2, Entries: 100, MemoryBytes: 1 << 20, SpillBytes: 1 << 20,
		})
		started := make(chan struct{})
		release := make(chan struct{})
		blocked := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			close(started)
			select {
			case <-ctx.Done():
				return ScanResult{}, ctx.Err()
			case <-release:
				return ScanResult{}, NewPermanentScanError(errors.New("done"))
			}
		})
		firstResult := make(chan error, 1)
		go func() {
			_, err := store.ListChildren(context.Background(), firstID, session, ScanOptions{}, blocked)
			firstResult <- err
		}()
		<-started
		never := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
			t.Fatal("active-scan rejection reached scanner")
			return ScanResult{}, nil
		})
		if _, err := store.ListChildren(context.Background(), secondID, session, ScanOptions{}, never); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("active scan admission = %v", err)
		}
		close(release)
		<-firstResult

		workSession, _ := NewBudgetAccount("work-session", BudgetLimits{
			ActiveScans: 1, ScanWork: 1, Entries: 100, MemoryBytes: 1 << 20, SpillBytes: 1 << 20,
		})
		workScanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 174, "file", 1))
		})
		if _, err := store.ListChildren(context.Background(), secondID, workSession, ScanOptions{}, workScanner); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("scan work admission = %v", err)
		}
	})
}

func TestStoreRejectsInvalidAndClosedOperations(t *testing.T) {
	process := generousBudget(t, "process")
	if _, err := NewCatalogStore(StoreConfig{}); err == nil {
		t.Fatal("empty store configuration was accepted")
	}
	if _, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: NewMemoryCatalogBackend(),
		ProcessBudget: process, ShareBudget: process, PageSealer: semanticTestCommitter{},
	}); err == nil {
		t.Fatal("aliased budgets were accepted")
	}
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	session := generousBudget(t, "session")
	scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		return ScanResult{}, errors.New("must not scan")
	})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ListChildren(cancelled, idValue[DirectoryID](1), session, ScanOptions{}, scanner); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing = %v", err)
	}
	if _, err := store.ListChildren(context.Background(), DirectoryID{}, session, ScanOptions{}, scanner); err == nil {
		t.Fatal("zero directory was accepted")
	}
	if _, err := store.ListChildren(context.Background(), idValue[DirectoryID](1), nil, ScanOptions{}, scanner); err == nil {
		t.Fatal("nil session was accepted")
	}
	if _, err := store.ListChildren(context.Background(), idValue[DirectoryID](1), session, ScanOptions{}, nil); err == nil {
		t.Fatal("nil scanner was accepted")
	}
	if _, err := store.ListChildren(context.Background(), idValue[DirectoryID](1), session, ScanOptions{}, scanner); err == nil {
		t.Fatal("unknown directory was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Directory(context.Background(), idValue[DirectoryID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed directory = %v", err)
	}
	if _, _, err := store.Page(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page = %v", err)
	}
	if _, _, err := store.Node(context.Background(), idValue[NodeID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed node = %v", err)
	}
}

func TestEmptyStreamingScanAndLowLevelAdmissionBranches(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 180, 181)
	emptyScanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		return ScanResult{OmittedCount: 1}, nil
	})
	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, emptyScanner)
	if err != nil || committed.EntryCount() != 0 || committed.OmittedCount() != 1 {
		t.Fatalf("empty streaming scan = %+v err=%v", committed, err)
	}
	if id, err := (randomCatalogIdentities{}).NewScanAttemptID(); err != nil || id.IsZero() {
		t.Fatalf("random attempt identity = %x err=%v", id, err)
	}
	if id, err := (randomCatalogIdentities{}).NewDirectoryGeneration(); err != nil || id.IsZero() {
		t.Fatalf("random generation identity = %x err=%v", id, err)
	}
	if _, err := (ScannedChild{}).nodeRecord(directory); err == nil {
		t.Fatal("kind-free scanned child was accepted")
	}
	both := scannedFile(t, 182, "both", 1)
	both.DirectoryID = idValue[DirectoryID](183)
	if _, err := both.nodeRecord(directory); err == nil {
		t.Fatal("ambiguous scanned child was accepted")
	}
	invalidDirectory := scannedDirectory(t, 184, "invalid-directory")
	candidate, _ := NewVersionCandidate([]byte("unexpected"))
	invalidDirectory.VersionCandidate = candidate
	if _, err := invalidDirectory.nodeRecord(directory); err == nil {
		t.Fatal("directory with file metadata was accepted")
	}

	hierarchy := BudgetHierarchy{
		Process: generousBudget(t, "sort-process"),
		Share:   generousBudget(t, "sort-share"),
		Session: generousBudget(t, "sort-session"),
	}
	meter, err := newAttemptResourceMeter(hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	defer meter.Close()
	if _, err := newExternalSorter(nil, meter, hierarchy, 1); err == nil {
		t.Fatal("storage-free sorter was accepted")
	}
	factory := NewFileSpillFactory(t.TempDir())
	if err := factory.Recover(context.Background(), idValue[ShareInstance](1)); err != nil {
		t.Fatal(err)
	}
	workspace, err := factory.NewWorkspace(context.Background(), SpillRequest{
		ShareInstance: idValue[ShareInstance](1), AttemptID: idValue[ScanAttemptID](1),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	sorter, err := newExternalSorter(workspace, meter, hierarchy, 0)
	if err != nil || sorter.runBytes != DefaultSortRunMemoryBytes {
		t.Fatalf("default sorter = %+v err=%v", sorter, err)
	}

	memory := NewMemoryCatalogBackend()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := memory.Recover(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled memory recovery = %v", err)
	}
	if err := memory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Recover(context.Background()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory recovery = %v", err)
	}
	collector := &scanChildCollector{work: scanWorkMeter{store: store, resources: meter}, count: MaxDirectoryEntries}
	if err := collector.Add(context.Background(), scannedFile(t, 185, "overflow", 1)); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("entry-limit collector = %v", err)
	}
}
