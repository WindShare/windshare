package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"sync"
)

var (
	ErrCatalogClosed      = errors.New("catalog store is closed")
	ErrGenerationConflict = errors.New("catalog directory generation conflicts with committed state")
)

const memoryObjectOverhead = uint64(64)

type BackendPreparation struct {
	Directory CommittedDirectory
	Usage     ResourceUsage
	Existing  bool
}

type FailurePreparation struct {
	Record   DirectoryFailureRecord
	Usage    ResourceUsage
	Existing bool
}

type BackendTransaction interface {
	PutDirectory(NodeRecord) error
	PutChild(NodeRecord) error
	PutPage(CatalogPage, SealedPageObject) error
	Prepare(context.Context) (BackendPreparation, error)
	Publish(context.Context) (CommittedDirectory, error)
	Abort() error
}

type CatalogBackend interface {
	Recover(context.Context) (ResourceUsage, error)
	BeginDirectory(context.Context, DirectoryID, DirectoryGeneration, ResourceMeter) (BackendTransaction, error)
	LoadDirectory(context.Context, DirectoryID) (CommittedDirectory, bool, error)
	LoadPage(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error)
	LoadPageObject(context.Context, DirectoryID, DirectoryGeneration, uint32) (SealedPageObject, bool, error)
	CommitFailure(context.Context, DirectoryFailureRecord, SealedFailureObject, ResourceMeter, func(ResourceUsage) error) (FailurePreparation, error)
	LoadFailureObject(context.Context, DirectoryID, ScanAttemptID) (SealedFailureObject, bool, error)
	ReplayFailures(context.Context, func(DirectoryFailureRecord, bool) error) error
	LoadNode(context.Context, NodeID) (NodeRecord, bool, error)
	Close() error
}

type memoryDirectory struct {
	committed     CommittedDirectory
	digest        [sha256.Size]byte
	directoryNode []byte
	children      map[NodeID][]byte
	pages         map[uint32][]byte
	pageObjects   map[uint32][]byte
	usage         ResourceUsage
}

type MemoryCatalogBackend struct {
	mu          sync.RWMutex
	closed      bool
	directories map[DirectoryID]memoryDirectory
	nodes       map[NodeID][]byte
	failures    map[DirectoryID]map[ScanAttemptID]memoryFailure
}

func NewMemoryCatalogBackend() *MemoryCatalogBackend {
	return &MemoryCatalogBackend{
		directories: make(map[DirectoryID]memoryDirectory),
		nodes:       make(map[NodeID][]byte),
		failures:    make(map[DirectoryID]map[ScanAttemptID]memoryFailure),
	}
}

func (b *MemoryCatalogBackend) Recover(ctx context.Context) (ResourceUsage, error) {
	if err := ctx.Err(); err != nil {
		return ResourceUsage{}, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ResourceUsage{}, ErrCatalogClosed
	}
	var usage ResourceUsage
	for _, directory := range b.directories {
		next, ok := addUsage(usage, directory.usage)
		if !ok {
			return ResourceUsage{}, ErrBudgetExceeded
		}
		usage = next
	}
	for _, attempts := range b.failures {
		for _, failure := range attempts {
			recovered, ok := addUsage(failure.usage, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes})
			if !ok {
				return ResourceUsage{}, ErrBudgetExceeded
			}
			next, ok := addUsage(usage, recovered)
			if !ok {
				return ResourceUsage{}, ErrBudgetExceeded
			}
			usage = next
		}
	}
	return usage, nil
}

func (b *MemoryCatalogBackend) BeginDirectory(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, meter ResourceMeter) (BackendTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory.IsZero() || generation.IsZero() || meter == nil {
		return nil, errors.New("catalog backend transaction requires identities and a staging meter")
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return nil, ErrCatalogClosed
	}
	return &memoryCatalogTransaction{
		backend: b, directory: directory, generation: generation, meter: meter,
		children: make(map[NodeID][]byte), pages: make(map[uint32][]byte), pageObjects: make(map[uint32][]byte),
		digest: sha256.New(),
	}, nil
}

func (b *MemoryCatalogBackend) LoadDirectory(ctx context.Context, directory DirectoryID) (CommittedDirectory, bool, error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return CommittedDirectory{}, false, ErrCatalogClosed
	}
	value, ok := b.directories[directory]
	return value.committed, ok, nil
}

func (b *MemoryCatalogBackend) LoadPage(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return CatalogPage{}, false, err
	}
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return CatalogPage{}, false, ErrCatalogClosed
	}
	value, ok := b.directories[directory]
	if !ok || value.committed.generation != generation {
		b.mu.RUnlock()
		return CatalogPage{}, false, nil
	}
	encoded, ok := value.pages[index]
	owned := append([]byte(nil), encoded...)
	b.mu.RUnlock()
	if !ok {
		return CatalogPage{}, false, nil
	}
	page, err := decodeCatalogPage(owned)
	if err != nil {
		return CatalogPage{}, false, err
	}
	return page, true, nil
}

func (b *MemoryCatalogBackend) LoadPageObject(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (SealedPageObject, bool, error) {
	if err := ctx.Err(); err != nil {
		return SealedPageObject{}, false, err
	}
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return SealedPageObject{}, false, ErrCatalogClosed
	}
	value, ok := b.directories[directory]
	if !ok || value.committed.generation != generation {
		b.mu.RUnlock()
		return SealedPageObject{}, false, nil
	}
	encoded, ok := value.pageObjects[index]
	owned := append([]byte(nil), encoded...)
	b.mu.RUnlock()
	if !ok {
		return SealedPageObject{}, false, nil
	}
	object, err := NewSealedPageObject(owned)
	if err != nil {
		return SealedPageObject{}, false, errors.Join(ErrCorruptCatalogStorage, err)
	}
	return object, true, nil
}

func (b *MemoryCatalogBackend) LoadNode(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return NodeRecord{}, false, err
	}
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return NodeRecord{}, false, ErrCatalogClosed
	}
	encoded, ok := b.nodes[id]
	owned := append([]byte(nil), encoded...)
	b.mu.RUnlock()
	if !ok {
		return NodeRecord{}, false, nil
	}
	record, err := decodeNodeRecord(owned)
	if err != nil {
		return NodeRecord{}, false, err
	}
	return record, true, nil
}

func (b *MemoryCatalogBackend) Close() error {
	b.mu.Lock()
	b.closed = true
	b.directories = nil
	b.nodes = nil
	b.failures = nil
	b.mu.Unlock()
	return nil
}

type memoryCatalogTransaction struct {
	mu              sync.Mutex
	backend         *MemoryCatalogBackend
	directory       DirectoryID
	generation      DirectoryGeneration
	meter           ResourceMeter
	directoryRecord NodeRecord
	directoryBytes  []byte
	children        map[NodeID][]byte
	pending         []NodeRecord
	pendingMemory   uint64
	pages           map[uint32][]byte
	pageObjects     map[uint32][]byte
	sequence        directorySequence
	digest          hash.Hash
	usage           ResourceUsage
	preparation     BackendPreparation
	prepared        bool
	finished        bool
}

func (t *memoryCatalogTransaction) ensureOpen() error {
	if t.finished {
		return errors.New("catalog backend transaction is already finished")
	}
	return nil
}

func (t *memoryCatalogTransaction) chargeBytes(encoded []byte) error {
	charge := ResourceUsage{MemoryBytes: memoryObjectOverhead + uint64(len(encoded))}
	if err := t.meter.Consume(charge); err != nil {
		return err
	}
	next, ok := addUsage(t.usage, charge)
	if !ok {
		return ErrBudgetExceeded
	}
	t.usage = next
	return nil
}

func (t *memoryCatalogTransaction) PutDirectory(record NodeRecord) (resultErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	directory, ok := record.DirectoryID()
	if t.prepared || !record.valid() || !ok || directory != t.directory || t.directoryRecord.valid() {
		return errors.New("catalog transaction directory record does not match its target")
	}
	temporary := ResourceUsage{MemoryBytes: record.EstimatedMemoryBytes()}
	if err := t.meter.Consume(temporary); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, t.meter.Release(temporary))
	}()
	encoded, err := encodeNodeRecord(record)
	if err != nil {
		return err
	}
	if err := t.chargeBytes(encoded); err != nil {
		return err
	}
	t.directoryRecord = record
	t.directoryBytes = encoded
	hashCatalogObject(t.digest, 1, encoded)
	return nil
}

func (t *memoryCatalogTransaction) PutChild(record NodeRecord) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if t.prepared || !record.valid() || record.Parent() != t.directory || len(t.pending) >= MaxCatalogPageEntries {
		return errors.New("catalog transaction child is invalid, unordered, or has another parent")
	}
	if _, exists := t.children[record.NodeID()]; exists {
		return ErrGenerationConflict
	}
	temporary := ResourceUsage{MemoryBytes: record.EstimatedMemoryBytes()}
	if err := t.meter.Consume(temporary); err != nil {
		return err
	}
	encoded, err := encodeNodeRecord(record)
	if err != nil {
		return err
	}
	if err := t.chargeBytes(encoded); err != nil {
		return err
	}
	if err := t.meter.Consume(ResourceUsage{Entries: 1}); err != nil {
		return err
	}
	t.children[record.NodeID()] = encoded
	t.pending = append(t.pending, record)
	t.pendingMemory += temporary.MemoryBytes
	t.usage.Entries++
	hashCatalogObject(t.digest, 2, encoded)
	return nil
}

func (t *memoryCatalogTransaction) PutPage(page CatalogPage, object SealedPageObject) (resultErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return err
	}
	if t.prepared || page.DirectoryID() != t.directory || page.Generation() != t.generation ||
		object.IsZero() || object.Commitment() != page.Commitment() {
		return errors.New("catalog transaction page does not match its target")
	}
	if len(t.pending) != len(page.entries) {
		return errors.New("catalog transaction page is not aligned with its private records")
	}
	for index, record := range t.pending {
		if !record.MatchesEntry(page.entries[index]) {
			return errors.New("catalog transaction page changed a private child record")
		}
	}
	if err := t.sequence.accept(page); err != nil {
		return err
	}
	temporary := ResourceUsage{MemoryBytes: page.EstimatedMemoryBytes()}
	if err := t.meter.Consume(temporary); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, t.meter.Release(temporary))
	}()
	encoded, err := encodeCatalogPage(page)
	if err != nil {
		return err
	}
	if err := t.chargeBytes(encoded); err != nil {
		return err
	}
	objectBytes := object.Bytes()
	if err := t.chargeBytes(objectBytes); err != nil {
		return err
	}
	t.pages[page.pageIndex] = encoded
	t.pageObjects[page.pageIndex] = objectBytes
	if err := t.meter.Release(ResourceUsage{MemoryBytes: t.pendingMemory}); err != nil {
		return err
	}
	t.pendingMemory = 0
	t.pending = t.pending[:0]
	hashCatalogObject(t.digest, 3, encoded)
	hashCatalogObject(t.digest, 4, objectBytes)
	return nil
}

func (t *memoryCatalogTransaction) Prepare(ctx context.Context) (BackendPreparation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return BackendPreparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackendPreparation{}, err
	}
	if !t.directoryRecord.valid() || len(t.pending) != 0 {
		return BackendPreparation{}, errors.New("catalog transaction is incomplete")
	}
	committed, err := t.sequence.finish()
	if err != nil {
		return BackendPreparation{}, err
	}
	if committed.directoryID != t.directory || committed.generation != t.generation || committed.entryCount != t.usage.Entries {
		return BackendPreparation{}, errors.New("catalog transaction metadata is inconsistent")
	}
	var digest [sha256.Size]byte
	copy(digest[:], t.digest.Sum(nil))
	t.backend.mu.RLock()
	existing, exists := t.backend.directories[t.directory]
	t.backend.mu.RUnlock()
	if exists && existing.digest != digest {
		return BackendPreparation{}, ErrGenerationConflict
	}
	t.preparation = BackendPreparation{Directory: committed, Usage: t.usage, Existing: exists}
	t.prepared = true
	return t.preparation, nil
}

func (t *memoryCatalogTransaction) Publish(ctx context.Context) (CommittedDirectory, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureOpen(); err != nil {
		return CommittedDirectory{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, err
	}
	if !t.prepared {
		return CommittedDirectory{}, errors.New("catalog transaction must be prepared before publication")
	}
	if t.preparation.Existing {
		t.finished = true
		return t.preparation.Directory, nil
	}
	var digest [sha256.Size]byte
	copy(digest[:], t.digest.Sum(nil))
	t.backend.mu.Lock()
	defer t.backend.mu.Unlock()
	if t.backend.closed {
		return CommittedDirectory{}, ErrCatalogClosed
	}
	if existing, exists := t.backend.directories[t.directory]; exists {
		if existing.digest != digest {
			return CommittedDirectory{}, ErrGenerationConflict
		}
		t.finished = true
		return existing.committed, nil
	}
	if existing, exists := t.backend.nodes[t.directoryRecord.NodeID()]; exists && !equalEncoded(existing, t.directoryBytes) {
		return CommittedDirectory{}, fmt.Errorf("%w: node %x", ErrGenerationConflict, t.directoryRecord.NodeID())
	}
	for id, encoded := range t.children {
		if existing, exists := t.backend.nodes[id]; exists && !equalEncoded(existing, encoded) {
			return CommittedDirectory{}, fmt.Errorf("%w: node %x", ErrGenerationConflict, id)
		}
	}
	t.backend.nodes[t.directoryRecord.NodeID()] = append([]byte(nil), t.directoryBytes...)
	for id, encoded := range t.children {
		t.backend.nodes[id] = append([]byte(nil), encoded...)
	}
	t.backend.directories[t.directory] = memoryDirectory{
		committed: t.preparation.Directory, digest: digest,
		directoryNode: append([]byte(nil), t.directoryBytes...),
		children:      cloneEncodedMap(t.children), pages: clonePageMap(t.pages),
		pageObjects: clonePageMap(t.pageObjects), usage: t.usage,
	}
	t.finished = true
	return t.preparation.Directory, nil
}

func (t *memoryCatalogTransaction) Abort() error {
	t.mu.Lock()
	t.finished = true
	t.children = nil
	t.pages = nil
	t.pageObjects = nil
	t.pending = nil
	t.mu.Unlock()
	return nil
}

func hashCatalogObject(target hash.Hash, kind byte, encoded []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(encoded)))
	_, _ = target.Write([]byte{kind})
	_, _ = target.Write(size[:])
	_, _ = target.Write(encoded)
}

func equalEncoded(left, right []byte) bool {
	return string(left) == string(right)
}

func cloneEncodedMap(source map[NodeID][]byte) map[NodeID][]byte {
	result := make(map[NodeID][]byte, len(source))
	for id, encoded := range source {
		result[id] = append([]byte(nil), encoded...)
	}
	return result
}

func clonePageMap(source map[uint32][]byte) map[uint32][]byte {
	result := make(map[uint32][]byte, len(source))
	for index, encoded := range source {
		result[index] = append([]byte(nil), encoded...)
	}
	return result
}
