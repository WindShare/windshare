package catalog

import (
	"context"
	"errors"
	"testing"
)

type commitFaultTransaction struct {
	putDirectoryErr error
	putChildErr     error
	putPageErr      error
	prepareErr      error
	publishErr      error
	existing        bool
	aborted         bool
}

func (t *commitFaultTransaction) PutDirectory(NodeRecord) error               { return t.putDirectoryErr }
func (t *commitFaultTransaction) PutChild(NodeRecord) error                   { return t.putChildErr }
func (t *commitFaultTransaction) PutPage(CatalogPage, SealedPageObject) error { return t.putPageErr }
func (t *commitFaultTransaction) Prepare(context.Context) (BackendPreparation, error) {
	if t.prepareErr != nil {
		return BackendPreparation{}, t.prepareErr
	}
	return BackendPreparation{Existing: t.existing}, nil
}
func (t *commitFaultTransaction) Publish(context.Context) (CommittedDirectory, error) {
	return CommittedDirectory{}, t.publishErr
}
func (t *commitFaultTransaction) Abort() error {
	t.aborted = true
	return nil
}

type commitFaultBackend struct {
	CatalogBackend
	beginErr error
	tx       *commitFaultTransaction
}

func (b *commitFaultBackend) BeginDirectory(context.Context, DirectoryID, DirectoryGeneration, ResourceMeter) (BackendTransaction, error) {
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}
func (b *commitFaultBackend) LoadDirectory(context.Context, DirectoryID) (CommittedDirectory, bool, error) {
	return CommittedDirectory{}, false, nil
}

type faultNodeSource struct {
	count      uint64
	openErr    error
	releaseErr error
	iterator   *faultNodeIterator
}

func (s faultNodeSource) Count() uint64 { return s.count }
func (s faultNodeSource) Open(context.Context) (NodeRecordIterator, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return s.iterator, nil
}
func (s faultNodeSource) Release(ResourceMeter) error { return s.releaseErr }

type faultNodeIterator struct {
	records  []NodeRecord
	index    int
	nextErr  error
	closeErr error
	extra    *NodeRecord
}

func (i *faultNodeIterator) Next(context.Context) (NodeRecord, bool, error) {
	if i.nextErr != nil {
		err := i.nextErr
		i.nextErr = nil
		return NodeRecord{}, false, err
	}
	if i.index < len(i.records) {
		record := i.records[i.index]
		i.index++
		return record, true, nil
	}
	if i.extra != nil {
		record := *i.extra
		i.extra = nil
		return record, true, nil
	}
	return NodeRecord{}, false, nil
}
func (i *faultNodeIterator) Close() error { return i.closeErr }

type failingCommitter struct {
	err error
}

func (c failingCommitter) Seal(PageCommitInput) (SealedPageObject, error) {
	return SealedPageObject{}, c.err
}
func (c failingCommitter) SealFailure(DirectoryFailureRecord) (SealedFailureObject, error) {
	return SealedFailureObject{}, c.err
}

func faultCommitFixture(t *testing.T) (NodeRecord, NodeRecord) {
	t.Helper()
	root := idValue[DirectoryID](190)
	directory := idValue[DirectoryID](191)
	parentLocator, _ := NewLocator(0, "directory")
	parentIdentity, _ := NewSourceIdentity([]byte("directory"))
	parent, err := NewDirectoryNodeRecord(directory, root, "directory", parentLocator, parentIdentity, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := scannedFile(t, 192, "file", 1).nodeRecord(directory)
	if err != nil {
		t.Fatal(err)
	}
	return parent, child
}

func newFaultCommitStore(t *testing.T, backend CatalogBackend, sealer PageSealer) *CatalogStore {
	t.Helper()
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: backend,
		ProcessBudget: generousBudget(t, "process"), ShareBudget: generousBudget(t, "share"),
		PageSealer: sealer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCommitPipelineFaultMatrixAlwaysAborts(t *testing.T) {
	injected := errors.New("injected")
	parent, child := faultCommitFixture(t)
	tests := []struct {
		name     string
		beginErr error
		tx       *commitFaultTransaction
		source   faultNodeSource
		sealer   PageSealer
	}{
		{name: "begin", beginErr: injected},
		{name: "put directory", tx: &commitFaultTransaction{putDirectoryErr: injected}},
		{name: "open source", tx: &commitFaultTransaction{}, source: faultNodeSource{count: 1, openErr: injected}},
		{name: "read source", tx: &commitFaultTransaction{}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{nextErr: injected}}},
		{name: "put child", tx: &commitFaultTransaction{putChildErr: injected}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
		{name: "commit page", tx: &commitFaultTransaction{}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}, sealer: failingCommitter{err: injected}},
		{name: "put page", tx: &commitFaultTransaction{putPageErr: injected}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
		{name: "close source", tx: &commitFaultTransaction{}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}, closeErr: injected}}},
		{name: "release source", tx: &commitFaultTransaction{}, source: faultNodeSource{count: 1, releaseErr: injected, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
		{name: "prepare", tx: &commitFaultTransaction{prepareErr: injected}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
		{name: "publish existing", tx: &commitFaultTransaction{existing: true, publishErr: injected}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
		{name: "publish new", tx: &commitFaultTransaction{publishErr: injected}, source: faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := test.tx
			if transaction == nil {
				transaction = &commitFaultTransaction{}
			}
			backend := &commitFaultBackend{
				CatalogBackend: NewMemoryCatalogBackend(), beginErr: test.beginErr, tx: transaction,
			}
			sealer := test.sealer
			if sealer == nil {
				sealer = semanticTestCommitter{}
			}
			store := newFaultCommitStore(t, backend, sealer)
			defer store.Close()
			source := test.source
			if source.count == 0 && source.openErr == nil {
				source = faultNodeSource{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}}}
			}
			commit := DirectoryCommit{
				directory: parent, generation: idValue[DirectoryGeneration](193), children: source,
			}
			if _, err := store.CommitDirectory(context.Background(), commit, generousBudget(t, "session")); !errors.Is(err, injected) {
				t.Fatalf("fault result = %v", err)
			}
			if test.beginErr == nil && !transaction.aborted {
				t.Fatal("failed pipeline did not abort its backend transaction")
			}
		})
	}
}

func TestStagePagesRejectsMalformedSources(t *testing.T) {
	parent, child := faultCommitFixture(t)
	otherParent := idValue[DirectoryID](194)
	wrongParent, _ := scannedFile(t, 195, "wrong", 1).nodeRecord(otherParent)
	unsortedA, _ := scannedFile(t, 196, "zeta", 1).nodeRecord(parent.directoryID)
	unsortedB, _ := scannedFile(t, 197, "alpha", 1).nodeRecord(parent.directoryID)
	injected := errors.New("malformed source")
	tests := []faultNodeSource{
		{count: 1, iterator: &faultNodeIterator{}},
		{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{wrongParent}}},
		{count: 2, iterator: &faultNodeIterator{records: []NodeRecord{unsortedA, unsortedB}}},
		{count: 1, iterator: &faultNodeIterator{records: []NodeRecord{child}, extra: &child}},
		{count: 1, iterator: &faultNodeIterator{nextErr: injected}},
	}
	for index, source := range tests {
		backend := &commitFaultBackend{CatalogBackend: NewMemoryCatalogBackend(), tx: &commitFaultTransaction{}}
		store := newFaultCommitStore(t, backend, semanticTestCommitter{})
		commit := DirectoryCommit{
			directory: parent, generation: idValue[DirectoryGeneration](byte(200 + index)), children: source,
		}
		if _, err := store.CommitDirectory(context.Background(), commit, generousBudget(t, "session")); err == nil {
			t.Fatalf("malformed source %d was accepted", index)
		}
		_ = store.Close()
	}
}
