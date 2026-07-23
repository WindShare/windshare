package catalog

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
)

type scriptedResourceMeter struct {
	consumeCalls  int
	releaseCalls  int
	failConsumeAt int
	failReleaseAt int
	err           error
}

func (m *scriptedResourceMeter) Consume(ResourceUsage) error {
	m.consumeCalls++
	if m.consumeCalls == m.failConsumeAt {
		return m.err
	}
	return nil
}

func (m *scriptedResourceMeter) Release(ResourceUsage) error {
	m.releaseCalls++
	if m.releaseCalls == m.failReleaseAt {
		return m.err
	}
	return nil
}

func TestMemoryTransactionRejectsOrderingMeterAndLifecycleFaults(t *testing.T) {
	injected := errors.New("meter fault")
	share := idValue[ShareInstance](1)
	directoryID := idValue[DirectoryID](101)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](100), "memory-fault", 1)
	child, _ := scannedFile(t, 102, "child", 1).nodeRecord(directoryID)
	generation := idValue[DirectoryGeneration](103)

	t.Run("directory meter", func(t *testing.T) {
		for _, failAt := range []int{1, 2} {
			backend := NewMemoryCatalogBackend()
			meter := &scriptedResourceMeter{failConsumeAt: failAt, err: injected}
			transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
			if err != nil {
				t.Fatal(err)
			}
			if err := transaction.PutDirectory(directory); !errors.Is(err, injected) {
				t.Fatalf("directory meter fault %d = %v", failAt, err)
			}
			_ = transaction.Abort()
		}
	})

	t.Run("child meter", func(t *testing.T) {
		for _, failAt := range []int{1, 2, 3} {
			backend := NewMemoryCatalogBackend()
			realMeter := backendTestMeter(t)
			transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, realMeter)
			if err != nil {
				t.Fatal(err)
			}
			if err := transaction.PutDirectory(directory); err != nil {
				t.Fatal(err)
			}
			transaction.(*memoryCatalogTransaction).meter = &scriptedResourceMeter{failConsumeAt: failAt, err: injected}
			if err := transaction.PutChild(child); !errors.Is(err, injected) {
				t.Fatalf("child meter fault %d = %v", failAt, err)
			}
			_ = transaction.Abort()
			realMeter.Close()
		}
	})

	t.Run("page validation and meter", func(t *testing.T) {
		newTransaction := func(t *testing.T) (*memoryCatalogTransaction, *attemptResourceMeter) {
			backend := NewMemoryCatalogBackend()
			meter := backendTestMeter(t)
			transaction, _ := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
			if err := transaction.PutDirectory(directory); err != nil {
				t.Fatal(err)
			}
			if err := transaction.PutChild(child); err != nil {
				t.Fatal(err)
			}
			return transaction.(*memoryCatalogTransaction), meter
		}
		makePage := func(t *testing.T, entries []Entry) CatalogPage {
			page, err := NewCatalogPage(CatalogPageSpec{
				ShareInstance: share, DirectoryID: directoryID, Generation: generation,
				Entries: entries, Terminal: true,
			}, semanticTestCommitter{})
			if err != nil {
				t.Fatal(err)
			}
			return page
		}
		transaction, meter := newTransaction(t)
		page := makePage(t, nil)
		if err := transaction.PutPage(page, mustTestPageObject(t, page)); err == nil {
			t.Fatal("page without private record alignment succeeded")
		}
		_ = transaction.Abort()
		meter.Close()

		transaction, meter = newTransaction(t)
		other, _ := scannedFile(t, 104, "other", 1).nodeRecord(directoryID)
		page = makePage(t, []Entry{other.Entry()})
		if err := transaction.PutPage(page, mustTestPageObject(t, page)); err == nil {
			t.Fatal("page changed its private record")
		}
		_ = transaction.Abort()
		meter.Close()

		for _, fault := range []struct {
			consumeAt int
			releaseAt int
		}{
			{consumeAt: 1}, {consumeAt: 2}, {releaseAt: 1},
		} {
			transaction, meter = newTransaction(t)
			transaction.meter = &scriptedResourceMeter{
				failConsumeAt: fault.consumeAt, failReleaseAt: fault.releaseAt, err: injected,
			}
			page = makePage(t, []Entry{child.Entry()})
			if err := transaction.PutPage(page, mustTestPageObject(t, page)); !errors.Is(err, injected) {
				t.Fatalf("page meter fault %+v = %v", fault, err)
			}
			_ = transaction.Abort()
			meter.Close()
		}
	})

	t.Run("sequence and lifecycle", func(t *testing.T) {
		backend := NewMemoryCatalogBackend()
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction, _ := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
		if err := transaction.PutDirectory(directory); err != nil {
			t.Fatal(err)
		}
		if err := transaction.PutChild(child); err != nil {
			t.Fatal(err)
		}
		if err := transaction.PutChild(child); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("duplicate private node = %v", err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := transaction.Prepare(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled memory prepare = %v", err)
		}
		if _, err := transaction.Publish(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled memory publish = %v", err)
		}
		if _, err := transaction.Publish(context.Background()); err == nil {
			t.Fatal("unprepared memory transaction published")
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
		if err := transaction.PutChild(child); err == nil {
			t.Fatal("finished memory transaction accepted a child")
		}
		if err := transaction.PutPage(CatalogPage{}, SealedPageObject{}); err == nil {
			t.Fatal("finished memory transaction accepted a page")
		}
		if _, err := transaction.Prepare(context.Background()); err == nil {
			t.Fatal("finished memory transaction prepared")
		}
		if _, err := transaction.Publish(context.Background()); err == nil {
			t.Fatal("finished memory transaction published")
		}
	})

	t.Run("incomplete and inconsistent", func(t *testing.T) {
		backend := NewMemoryCatalogBackend()
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction, _ := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
		if err := transaction.PutDirectory(directory); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Prepare(context.Background()); !errors.Is(err, ErrPageSequence) {
			t.Fatalf("generation without terminal page = %v", err)
		}
		_ = transaction.Abort()

		valid := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{child}, meter).(*memoryCatalogTransaction)
		valid.usage.Entries++
		if _, err := valid.Prepare(context.Background()); err == nil {
			t.Fatal("inconsistent memory generation prepared")
		}
		_ = valid.Abort()

		overflow := &memoryCatalogTransaction{meter: &scriptedResourceMeter{}, usage: ResourceUsage{MemoryBytes: math.MaxUint64}}
		if err := overflow.chargeBytes(nil); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("memory transaction accounting overflow = %v", err)
		}
	})

	t.Run("closed publication and directory collision", func(t *testing.T) {
		backend := NewMemoryCatalogBackend()
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{child}, meter)
		if _, err := transaction.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := backend.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Publish(context.Background()); !errors.Is(err, ErrCatalogClosed) {
			t.Fatalf("closed memory publication = %v", err)
		}
		_ = transaction.Abort()

		backend = NewMemoryCatalogBackend()
		transaction = stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{child}, meter)
		if _, err := transaction.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		backend.nodes[directory.NodeID()] = []byte("foreign")
		if _, err := transaction.Publish(context.Background()); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("memory directory-node collision = %v", err)
		}
		_ = transaction.Abort()
	})
}

func TestCatalogCodecRejectsHostilePrivateRecordsAndPages(t *testing.T) {
	parent := idValue[DirectoryID](110)
	file, _ := scannedFile(t, 111, "file", 1).nodeRecord(parent)
	directory, _ := scannedDirectory(t, 112, "directory").nodeRecord(parent)
	modified := storedModifiedTime{Present: true, Precision: 99}
	for name, entry := range map[string]storedEntry{
		"modified":       {Kind: uint8(NodeKindFile), ID: file.fileID.Bytes(), Name: "file", Modified: modified},
		"directory-id":   {Kind: uint8(NodeKindDirectory), ID: []byte{1}, Name: "directory"},
		"directory-name": {Kind: uint8(NodeKindDirectory), ID: directory.directoryID.Bytes(), Name: "."},
		"file-id":        {Kind: uint8(NodeKindFile), ID: []byte{1}, Name: "file"},
		"file-name":      {Kind: uint8(NodeKindFile), ID: file.fileID.Bytes(), Name: "."},
	} {
		t.Run("entry-"+name, func(t *testing.T) {
			if _, err := restoreEntry(entry); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("hostile entry accepted: %v", err)
			}
		})
	}
	if _, err := encodeNodeRecord(NodeRecord{}); err == nil {
		t.Fatal("invalid node encoded")
	}
	if _, err := encodeCatalogPage(CatalogPage{}); err == nil {
		t.Fatal("invalid page encoded")
	}

	base := storedNode{
		Schema: catalogStorageSchema, Kind: uint8(NodeKindFile), ID: file.nodeID.Bytes(),
		Parent: parent.Bytes(), Name: file.name, RootSlot: uint16(file.locator.rootSlot),
		RelativePath: file.locator.relativePath, SourceIdentity: file.sourceIdentity.Bytes(),
		VersionCandidate: file.versionCandidate.Bytes(), ExpectedSize: file.expectedSize,
	}
	mutations := map[string]func(*storedNode){
		"node-id":   func(value *storedNode) { value.ID = []byte{1} },
		"modified":  func(value *storedNode) { value.Modified = modified },
		"parent":    func(value *storedNode) { value.Parent = []byte{1} },
		"locator":   func(value *storedNode) { value.RootSlot = MaxRootSlots },
		"source":    func(value *storedNode) { value.SourceIdentity = nil },
		"candidate": func(value *storedNode) { value.VersionCandidate = nil },
		"file-name": func(value *storedNode) { value.Name = "." },
		"directory-name": func(value *storedNode) {
			value.Kind = uint8(NodeKindDirectory)
			value.ID = directory.nodeID.Bytes()
			value.VersionCandidate = nil
			value.ExpectedSize = 0
			value.Name = "."
		},
		"kind": func(value *storedNode) { value.Kind = 99 },
	}
	for name, mutate := range mutations {
		t.Run("node-"+name, func(t *testing.T) {
			value := base
			mutate(&value)
			encoded, err := catalogStorageEnc.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeNodeRecord(encoded); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("hostile node accepted: %v", err)
			}
		})
	}

	validPage, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](1), DirectoryID: parent,
		Generation: idValue[DirectoryGeneration](113), Entries: []Entry{file.Entry()}, Terminal: true,
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := encodeCatalogPage(validPage)
	var stored storedPage
	if err := catalogStorageDec.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	pageMutations := map[string]func(*storedPage){
		"schema":   func(value *storedPage) { value.Schema++ },
		"identity": func(value *storedPage) { value.ShareInstance = nil },
		"entry":    func(value *storedPage) { value.Entries[0].Name = "." },
		"order": func(value *storedPage) {
			second := value.Entries[0]
			second.ID = idValue[FileID](114).Bytes()
			value.Entries = []storedEntry{second, value.Entries[0]}
		},
	}
	for name, mutate := range pageMutations {
		t.Run("page-"+name, func(t *testing.T) {
			value := stored
			value.Entries = append([]storedEntry(nil), stored.Entries...)
			mutate(&value)
			encoded, err := catalogStorageEnc.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeCatalogPage(encoded); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("hostile page accepted: %v", err)
			}
		})
	}
	if err := decodeCanonicalStorage([]byte{0xff}, &storedPage{}); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("invalid durable CBOR = %v", err)
	}
}

func TestDirectorySequenceRejectsCrossPageIdentityOrderAndLimitViolations(t *testing.T) {
	share := idValue[ShareInstance](1)
	directory := idValue[DirectoryID](120)
	generation := idValue[DirectoryGeneration](121)
	entryA, _ := NewFileEntry(idValue[FileID](122), "alpha", 1, ModifiedTime{})
	entryZ, _ := NewFileEntry(idValue[FileID](123), "zeta", 1, ModifiedTime{})
	page := func(t *testing.T, spec CatalogPageSpec) CatalogPage {
		t.Helper()
		value, err := NewCatalogPage(spec, semanticTestCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{entryZ},
	})
	terminal := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: []Entry{entryA}, Terminal: true,
	})
	var sequence directorySequence
	if err := sequence.accept(CatalogPage{}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("commitment-free page = %v", err)
	}
	if err := sequence.accept(first); err != nil {
		t.Fatal(err)
	}
	if err := sequence.accept(terminal); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("cross-page order = %v", err)
	}

	identityChange := page(t, CatalogPageSpec{
		ShareInstance: idValue[ShareInstance](2), DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: first.Commitment(), Entries: []Entry{entryZ}, Terminal: true,
	})
	sequence = directorySequence{}
	_ = sequence.accept(first)
	if err := sequence.accept(identityChange); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("generation identity change = %v", err)
	}

	gap := terminal
	gap.pageIndex = 2
	sequence = directorySequence{}
	_ = sequence.accept(first)
	if err := sequence.accept(gap); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("page gap = %v", err)
	}

	sequence = directorySequence{identitySet: true, committed: CommittedDirectory{
		shareInstance: share, directoryID: directory, generation: generation, entryCount: MaxDirectoryEntries,
	}}
	if err := sequence.accept(first); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("entry limit = %v", err)
	}
	sequence = directorySequence{}
	overOmitted := first
	overOmitted.terminal = true
	overOmitted.omittedCount = MaxDirectoryEntries
	if err := sequence.accept(overOmitted); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("omitted limit = %v", err)
	}

	sequence = directorySequence{}
	validTerminal := first
	validTerminal.terminal = true
	if err := sequence.accept(validTerminal); err != nil {
		t.Fatal(err)
	}
	if err := sequence.accept(validTerminal); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("page after terminal = %v", err)
	}

	if _, err := NewDirectorySnapshot([]CatalogPage{validTerminal, validTerminal}); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("early terminal snapshot = %v", err)
	}
	upper, _ := NewFileEntry(idValue[FileID](124), "Alpha", 1, ModifiedTime{})
	lower, _ := NewFileEntry(idValue[FileID](125), "alpha", 1, ModifiedTime{})
	nameFirst := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{upper},
	})
	nameSecond := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: nameFirst.Commitment(), Entries: []Entry{lower}, Terminal: true,
	})
	if _, err := NewDirectorySnapshot([]CatalogPage{nameFirst, nameSecond}); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("cross-page duplicate name = %v", err)
	}
	sharedID := idValue[FileID](126)
	sharedAlpha, _ := NewFileEntry(sharedID, "alpha", 1, ModifiedTime{})
	sharedBeta, _ := NewFileEntry(sharedID, "beta", 1, ModifiedTime{})
	nodeFirst := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, Entries: []Entry{sharedAlpha},
	})
	nodeSecond := page(t, CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generation, PageIndex: 1,
		Previous: nodeFirst.Commitment(), Entries: []Entry{sharedBeta}, Terminal: true,
	})
	if _, err := NewDirectorySnapshot([]CatalogPage{nodeFirst, nodeSecond}); !errors.Is(err, ErrSiblingCollision) {
		t.Fatalf("cross-page duplicate node = %v", err)
	}

	snapshot, err := NewDirectorySnapshot([]CatalogPage{validTerminal})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ShareInstance() != share || snapshot.DirectoryID() != directory || snapshot.Generation() != generation {
		t.Fatal("snapshot identity accessors changed committed identity")
	}
	other := snapshot
	other.pages = append([]CatalogPage(nil), snapshot.pages...)
	other.pages[0].commitment[0] ^= 0xff
	if snapshot.Equal(other) {
		t.Fatal("snapshots with different commitments compared equal")
	}
	if _, ok := snapshot.Page(math.MaxUint32); ok {
		t.Fatal("out-of-range snapshot page succeeded")
	}
	if (DirectorySnapshot{}).TerminalCommitment() != (PageCommitment{}) {
		t.Fatal("empty snapshot returned a terminal commitment")
	}
}

func TestSortMergePropagatesCursorWriterRemovalAndBudgetFaults(t *testing.T) {
	injected := errors.New("sort fault")
	hierarchy, meter := sorterBudget(t, 1<<20, 1<<20)
	defer meter.Close()
	empty := &fakeSpillObject{}
	framed := func(key string) *fakeSpillObject {
		data := framedSortBytes(uint32(len(key)), 0, []byte(key))
		return &fakeSpillObject{data: data, size: uint64(len(data))}
	}
	for name, test := range map[string]struct {
		workspace *fakeSpillWorkspace
		left      SpillObject
		right     SpillObject
	}{
		"right open": {workspace: &fakeSpillWorkspace{}, left: empty, right: &fakeSpillObject{openErr: injected}},
		"workspace":  {workspace: &fakeSpillWorkspace{createErr: injected}, left: empty, right: empty},
		"left read":  {workspace: &fakeSpillWorkspace{}, left: &fakeSpillObject{data: []byte{0}}, right: empty},
		"right read": {workspace: &fakeSpillWorkspace{}, left: empty, right: &fakeSpillObject{data: []byte{0}}},
		"duplicate":  {workspace: &fakeSpillWorkspace{}, left: framed("same"), right: framed("same")},
		"commit":     {workspace: &fakeSpillWorkspace{writer: &fakeSpillWriter{commitErr: injected}}, left: empty, right: empty},
		"remove":     {workspace: &fakeSpillWorkspace{}, left: &fakeSpillObject{removeErr: injected}, right: empty},
		"release":    {workspace: &fakeSpillWorkspace{}, left: &fakeSpillObject{size: 1}, right: empty},
	} {
		t.Run(name, func(t *testing.T) {
			sorter, _ := newExternalSorter(test.workspace, meter, hierarchy, 64)
			_, err := sorter.mergePair(context.Background(), test.left, test.right, true)
			if err == nil {
				t.Fatal("merge fault was hidden")
			}
		})
	}

	for name, writer := range map[string]io.Writer{
		"key":     &failNthWriter{failAt: 2, err: injected},
		"payload": &failNthWriter{failAt: 3, err: injected},
	} {
		t.Run("writer-"+name, func(t *testing.T) {
			if err := writeSortRecord(writer, sortRecord{key: []byte("key"), payload: []byte("payload")}, meter); !errors.Is(err, injected) {
				t.Fatalf("sort writer fault = %v", err)
			}
		})
	}

	for name, workspace := range map[string]*fakeSpillWorkspace{
		"create": {createErr: injected},
		"commit": {writer: &fakeSpillWriter{commitErr: injected}},
	} {
		t.Run("empty-finish-"+name, func(t *testing.T) {
			sorter, _ := newExternalSorter(workspace, meter, hierarchy, 64)
			if _, _, err := sorter.finish(context.Background(), true); !errors.Is(err, injected) {
				t.Fatalf("empty sorter fault = %v", err)
			}
		})
	}
}

type failNthWriter struct {
	writes int
	failAt int
	err    error
}

func (w *failNthWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return len(data), nil
}
