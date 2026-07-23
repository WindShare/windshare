package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

type NodeRecordIterator interface {
	Next(context.Context) (NodeRecord, bool, error)
	Close() error
}

type nodeRecordSource interface {
	Count() uint64
	Open(context.Context) (NodeRecordIterator, error)
	Release(ResourceMeter) error
}

type DirectoryCommit struct {
	directory    NodeRecord
	generation   DirectoryGeneration
	children     nodeRecordSource
	omittedCount uint64
	synthetic    bool
}

func (c DirectoryCommit) Directory() NodeRecord           { return c.directory }
func (c DirectoryCommit) Generation() DirectoryGeneration { return c.generation }
func (c DirectoryCommit) EntryCount() uint64              { return c.children.Count() }
func (c DirectoryCommit) OmittedCount() uint64            { return c.omittedCount }

type sliceNodeSource struct {
	records []NodeRecord
}

func newSliceNodeSource(records []NodeRecord) sliceNodeSource {
	return sliceNodeSource{records: append([]NodeRecord(nil), records...)}
}

func (s sliceNodeSource) Count() uint64 { return uint64(len(s.records)) }

func (s sliceNodeSource) Open(context.Context) (NodeRecordIterator, error) {
	return &sliceNodeIterator{records: s.records}, nil
}

func (s sliceNodeSource) Release(ResourceMeter) error { return nil }

type sliceNodeIterator struct {
	records []NodeRecord
	index   int
}

func (i *sliceNodeIterator) Next(ctx context.Context) (NodeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return NodeRecord{}, false, err
	}
	if i.index == len(i.records) {
		return NodeRecord{}, false, nil
	}
	record := i.records[i.index]
	i.index++
	return record, true, nil
}

func (i *sliceNodeIterator) Close() error {
	i.records = nil
	return nil
}

type attemptResourceMeter struct {
	mu          sync.Mutex
	reservation *BudgetReservation
}

func newAttemptResourceMeter(hierarchy BudgetHierarchy) (*attemptResourceMeter, error) {
	reservation, err := ReserveHierarchy(hierarchy, ResourceUsage{})
	if err != nil {
		return nil, err
	}
	return &attemptResourceMeter{reservation: reservation}, nil
}

func (m *attemptResourceMeter) Consume(usage ResourceUsage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservation == nil {
		return errors.New("catalog resource meter is closed")
	}
	return m.reservation.Grow(usage)
}

func (m *attemptResourceMeter) Release(usage ResourceUsage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservation == nil {
		return errors.New("catalog resource meter is closed")
	}
	return m.reservation.Shrink(usage)
}

func (m *attemptResourceMeter) retain(usage ResourceUsage, session *BudgetAccount) (*BudgetReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservation == nil {
		return nil, errors.New("catalog resource meter is closed")
	}
	if err := m.reservation.keep(usage); err != nil {
		return nil, err
	}
	if err := m.reservation.dropAccount(session); err != nil {
		return nil, err
	}
	retained := m.reservation
	m.reservation = nil
	return retained, nil
}

// reopen restores a zero-sized attempt reservation after a publication failure.
// Retention must precede publication, so a failed publish has already detached
// the old reservation even though the attempt still needs to persist its failure.
func (m *attemptResourceMeter) reopen(hierarchy BudgetHierarchy) error {
	reservation, err := ReserveHierarchy(hierarchy, ResourceUsage{})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservation != nil {
		reservation.Release()
		return errors.New("catalog resource meter is already open")
	}
	m.reservation = reservation
	return nil
}

func (m *attemptResourceMeter) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	reservation := m.reservation
	m.reservation = nil
	m.mu.Unlock()
	reservation.Release()
}

func (s *CatalogStore) CommitDirectory(ctx context.Context, commit DirectoryCommit, sessionBudget *BudgetAccount) (CommittedDirectory, error) {
	if err := validateDirectoryCommit(s.shareInstance, commit, false); err != nil {
		return CommittedDirectory{}, err
	}
	meter, err := newAttemptResourceMeter(s.hierarchy(sessionBudget))
	if err != nil {
		return CommittedDirectory{}, err
	}
	defer meter.Close()
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	committed, _, err := s.commitDirectoryLocked(ctx, commit, sessionBudget, meter)
	return committed, err
}

func (s *CatalogStore) CommitSyntheticRoot(ctx context.Context, commit DirectoryCommit, startupBudget *BudgetAccount) (CommittedRoot, error) {
	if err := validateDirectoryCommit(s.shareInstance, commit, true); err != nil {
		return CommittedRoot{}, err
	}
	meter, err := newAttemptResourceMeter(s.hierarchy(startupBudget))
	if err != nil {
		return CommittedRoot{}, err
	}
	defer meter.Close()
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if s.isClosed() {
		return CommittedRoot{}, ErrCatalogClosed
	}
	directoryID, _ := commit.directory.DirectoryID()
	s.mu.Lock()
	committedRoot := s.committedRoot
	s.mu.Unlock()
	if committedRoot != nil && (committedRoot.directory != directoryID || committedRoot.generation != commit.generation) {
		return CommittedRoot{}, ErrGenerationConflict
	}
	committed, reservation, err := s.commitDirectoryLocked(ctx, commit, startupBudget, meter)
	if err != nil {
		return CommittedRoot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committedRoot != nil {
		if s.committedRoot.directory != committed.directoryID || s.committedRoot.generation != committed.generation {
			return CommittedRoot{}, ErrGenerationConflict
		}
		return CommittedRoot{state: s.committedRoot}, nil
	}
	if reservation == nil {
		reservation = s.recoveredReservation
	}
	if reservation == nil || !reservation.active() {
		return CommittedRoot{}, fmt.Errorf("%w: synthetic root lacks a live local budget reservation", ErrCommittedRootUnavailable)
	}
	state := &committedRootState{
		store: s, share: s.shareInstance, directory: committed.directoryID,
		generation: committed.generation, reservation: reservation,
	}
	s.committedRoot = state
	return CommittedRoot{state: state}, nil
}

func validateDirectoryCommit(instance ShareInstance, commit DirectoryCommit, synthetic bool) error {
	directoryID, directoryKind := commit.directory.DirectoryID()
	if instance.IsZero() || !commit.directory.valid() || !directoryKind || commit.generation.IsZero() || commit.children == nil {
		return errors.New("catalog commit requires a valid directory, generation, and child source")
	}
	if commit.synthetic != synthetic || commit.directory.IsSyntheticRoot() != synthetic {
		return errors.New("catalog synthetic root must use its dedicated commit path")
	}
	if commit.children.Count() > MaxDirectoryEntries ||
		commit.omittedCount > MaxDirectoryEntries-commit.children.Count() {
		return fmt.Errorf("%w: directory %x exceeds its entry limit", ErrPageLimit, directoryID)
	}
	if synthetic {
		if commit.children.Count() == 0 || commit.children.Count() > MaxRootSlots || commit.omittedCount != 0 {
			return fmt.Errorf("catalog synthetic root must contain 1..%d selected roots without omissions", MaxRootSlots)
		}
	}
	return nil
}

func (s *CatalogStore) commitDirectoryLocked(
	ctx context.Context,
	commit DirectoryCommit,
	sessionBudget *BudgetAccount,
	meter *attemptResourceMeter,
) (committedResult CommittedDirectory, retainedResult *BudgetReservation, resultErr error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, nil, err
	}
	if s.isClosed() {
		return CommittedDirectory{}, nil, ErrCatalogClosed
	}
	directoryID, _ := commit.directory.DirectoryID()
	existing, found, err := s.backend.LoadDirectory(ctx, directoryID)
	if err != nil {
		return CommittedDirectory{}, nil, errors.Join(err, commit.children.Release(meter))
	}
	if found {
		validated, validateErr := s.validateExistingCommit(ctx, commit, existing, meter)
		return validated, nil, validateErr
	}
	transaction, err := s.backend.BeginDirectory(ctx, directoryID, commit.generation, meter)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, transaction.Abort())
		}
	}()
	if err := transaction.PutDirectory(commit.directory); err != nil {
		return CommittedDirectory{}, nil, err
	}
	iterator, err := commit.children.Open(ctx)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}
	sourceReleased := false
	iteratorClosed := false
	defer func() {
		if !iteratorClosed {
			resultErr = errors.Join(resultErr, iterator.Close())
		}
		if !sourceReleased {
			resultErr = errors.Join(resultErr, commit.children.Release(meter))
		}
	}()
	if err := s.stagePages(ctx, transaction, commit, iterator, sessionBudget); err != nil {
		return CommittedDirectory{}, nil, err
	}
	if err := iterator.Close(); err != nil {
		return CommittedDirectory{}, nil, err
	}
	iteratorClosed = true
	if err := commit.children.Release(meter); err != nil {
		return CommittedDirectory{}, nil, err
	}
	sourceReleased = true
	prepared, err := transaction.Prepare(ctx)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}
	if prepared.Existing {
		committed, err := transaction.Publish(ctx)
		published = err == nil
		return committed, nil, err
	}
	retained, err := meter.retain(prepared.Usage, sessionBudget)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}
	committed, err := transaction.Publish(ctx)
	if err != nil {
		retained.Release()
		return CommittedDirectory{}, nil, errors.Join(err, meter.reopen(s.hierarchy(sessionBudget)))
	}
	published = true
	s.mu.Lock()
	s.held = append(s.held, retained)
	s.mu.Unlock()
	return committed, retained, nil
}

func (s *CatalogStore) validateExistingCommit(
	ctx context.Context,
	commit DirectoryCommit,
	existing CommittedDirectory,
	meter ResourceMeter,
) (result CommittedDirectory, resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, commit.children.Release(meter))
	}()
	directoryID, _ := commit.directory.DirectoryID()
	if existing.ShareInstance() != s.shareInstance || existing.DirectoryID() != directoryID ||
		existing.Generation() != commit.generation || existing.EntryCount() != commit.children.Count() ||
		existing.OmittedCount() != commit.omittedCount {
		return CommittedDirectory{}, ErrGenerationConflict
	}
	durableDirectory, found, err := s.backend.LoadNode(ctx, commit.directory.NodeID())
	if err != nil {
		return CommittedDirectory{}, err
	}
	if !found || durableDirectory != commit.directory {
		return CommittedDirectory{}, ErrGenerationConflict
	}
	iterator, err := commit.children.Open(ctx)
	if err != nil {
		return CommittedDirectory{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, iterator.Close())
	}()
	var consumed uint64
	for pageIndex := uint32(0); pageIndex < existing.PageCount(); pageIndex++ {
		pageEntries, err := s.validateExistingPage(ctx, commit, directoryID, pageIndex, iterator)
		if err != nil {
			return CommittedDirectory{}, err
		}
		consumed += pageEntries
	}
	if consumed != existing.EntryCount() {
		return CommittedDirectory{}, ErrCorruptCatalogStorage
	}
	if err := requireExhaustedNodeSource(ctx, iterator); err != nil {
		return CommittedDirectory{}, errors.Join(ErrGenerationConflict, err)
	}
	return existing, nil
}

func (s *CatalogStore) validateExistingPage(
	ctx context.Context,
	commit DirectoryCommit,
	directoryID DirectoryID,
	pageIndex uint32,
	iterator NodeRecordIterator,
) (uint64, error) {
	page, pageFound, err := s.backend.LoadPage(ctx, directoryID, commit.generation, pageIndex)
	if err != nil {
		return 0, err
	}
	object, objectFound, err := s.backend.LoadPageObject(ctx, directoryID, commit.generation, pageIndex)
	if err != nil {
		return 0, err
	}
	if !pageFound || !objectFound || object.Commitment() != page.Commitment() ||
		page.ShareInstance() != s.shareInstance || page.DirectoryID() != directoryID ||
		page.Generation() != commit.generation || page.PageIndex() != pageIndex {
		return 0, ErrCorruptCatalogStorage
	}
	for _, entry := range page.entries {
		record, ok, err := iterator.Next(ctx)
		if err != nil {
			return 0, err
		}
		if !ok || !record.MatchesEntry(entry) {
			return 0, ErrGenerationConflict
		}
		durableRecord, found, err := s.backend.LoadNode(ctx, record.NodeID())
		if err != nil {
			return 0, err
		}
		if !found || durableRecord != record {
			return 0, ErrGenerationConflict
		}
	}
	return uint64(len(page.entries)), nil
}

func (s *CatalogStore) stagePages(
	ctx context.Context,
	transaction BackendTransaction,
	commit DirectoryCommit,
	iterator NodeRecordIterator,
	sessionBudget *BudgetAccount,
) error {
	count := commit.children.Count()
	if count == 0 {
		return s.stageEmptyPage(transaction, commit, sessionBudget)
	}
	state := pageStagingState{}
	for pageIndex := uint32(0); state.consumed < count; pageIndex++ {
		if err := s.stageNonEmptyPage(
			ctx, transaction, commit, iterator, sessionBudget, count, pageIndex, &state,
		); err != nil {
			return err
		}
	}
	return requireExhaustedNodeSource(ctx, iterator)
}

type pageStagingState struct {
	previous PageCommitment
	lastName string
	consumed uint64
}

func (s *CatalogStore) stageEmptyPage(transaction BackendTransaction, commit DirectoryCommit, sessionBudget *BudgetAccount) error {
	memory, err := ReserveHierarchy(
		s.hierarchy(sessionBudget),
		ResourceUsage{MemoryBytes: CatalogPageMemoryOverhead + MaxCatalogPageObjectBytes},
	)
	if err != nil {
		return err
	}
	defer memory.Release()
	page, object, err := sealCatalogPage(CatalogPageSpec{
		ShareInstance: s.shareInstance, DirectoryID: commit.directory.directoryID,
		Generation: commit.generation, Terminal: true, OmittedCount: commit.omittedCount,
	}, s.pageSealer)
	if err != nil {
		return err
	}
	return transaction.PutPage(page, object)
}

func (s *CatalogStore) stageNonEmptyPage(
	ctx context.Context,
	transaction BackendTransaction,
	commit DirectoryCommit,
	iterator NodeRecordIterator,
	sessionBudget *BudgetAccount,
	count uint64,
	pageIndex uint32,
	state *pageStagingState,
) error {
	memory, err := ReserveHierarchy(
		s.hierarchy(sessionBudget),
		ResourceUsage{MemoryBytes: CatalogPageMemoryOverhead + MaxCatalogPageObjectBytes},
	)
	if err != nil {
		return err
	}
	defer memory.Release()
	pageEntries := min(uint64(MaxCatalogPageEntries), count-state.consumed)
	entries, err := collectPageEntries(ctx, transaction, commit.directory.directoryID, iterator, memory, pageEntries, state)
	if err != nil {
		return err
	}
	terminal := state.consumed == count
	omittedCount := uint64(0)
	if terminal {
		omittedCount = commit.omittedCount
	}
	page, object, err := sealCatalogPage(CatalogPageSpec{
		ShareInstance: s.shareInstance, DirectoryID: commit.directory.directoryID, Generation: commit.generation,
		PageIndex: pageIndex, Previous: state.previous, Entries: entries, Terminal: terminal, OmittedCount: omittedCount,
	}, s.pageSealer)
	if err != nil {
		return err
	}
	if err := transaction.PutPage(page, object); err != nil {
		return err
	}
	state.previous = page.Commitment()
	return nil
}

func collectPageEntries(
	ctx context.Context,
	transaction BackendTransaction,
	parent DirectoryID,
	iterator NodeRecordIterator,
	memory *BudgetReservation,
	count uint64,
	state *pageStagingState,
) ([]Entry, error) {
	entries := make([]Entry, 0, count)
	for range count {
		record, ok, err := iterator.Next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok || !record.valid() || record.Parent() != parent {
			return nil, errors.New("catalog child source ended early or changed its parent")
		}
		entry := record.Entry()
		if state.consumed > 0 && state.lastName >= entry.Name() {
			return nil, ErrPageSequence
		}
		entryMemory := uint64(CatalogEntryMemoryOverhead + CatalogNameMemoryOverhead + len(entry.Name()))
		if err := memory.Grow(ResourceUsage{MemoryBytes: entryMemory}); err != nil {
			return nil, err
		}
		if err := transaction.PutChild(record); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		state.lastName = entry.Name()
		state.consumed++
	}
	return entries, nil
}

func requireExhaustedNodeSource(ctx context.Context, iterator NodeRecordIterator) error {
	if _, ok, err := iterator.Next(ctx); err != nil {
		return err
	} else if ok {
		return errors.New("catalog child source contains more records than declared")
	}
	return nil
}

var _ io.Closer = (*sliceNodeIterator)(nil)
