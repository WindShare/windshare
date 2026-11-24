package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type DirectoryCommit struct {
	directory    NodeRecord
	generation   DirectoryGeneration
	children     []NodeRecord
	omittedCount uint64
	synthetic    bool
}

func (c DirectoryCommit) Directory() NodeRecord           { return c.directory }
func (c DirectoryCommit) Generation() DirectoryGeneration { return c.generation }
func (c DirectoryCommit) EntryCount() uint64              { return uint64(len(c.children)) }
func (c DirectoryCommit) OmittedCount() uint64            { return c.omittedCount }

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

func (m *attemptResourceMeter) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reservation != nil {
		_ = m.reservation.keep(ResourceUsage{})
	}
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
	committed, _, err := s.commitDirectoryLocked(ctx, commit, sessionBudget, meter, false, nil)
	return committed, s.classifyDirectCommitBudgetError(err)
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
	s.mu.RLock()
	committedRoot := s.committedRoot
	s.mu.RUnlock()
	if committedRoot != nil && (committedRoot.directory != directoryID || committedRoot.generation != commit.generation) {
		return CommittedRoot{}, ErrGenerationConflict
	}
	committed, reservation, err := s.commitDirectoryLocked(ctx, commit, startupBudget, meter, false, nil)
	if err != nil {
		return CommittedRoot{}, s.classifyDirectCommitBudgetError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CommittedRoot{}, ErrCatalogClosed
	}
	if s.committedRoot != nil {
		if s.committedRoot.directory != committed.directoryID || s.committedRoot.generation != committed.generation {
			return CommittedRoot{}, ErrGenerationConflict
		}
		return CommittedRoot{state: s.committedRoot}, nil
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
	if instance.IsZero() || !commit.directory.valid() || !directoryKind || commit.generation.IsZero() {
		return errors.New("catalog commit requires a valid directory and generation")
	}
	if commit.synthetic != synthetic || commit.directory.IsSyntheticRoot() != synthetic {
		return errors.New("catalog synthetic root must use its dedicated commit path")
	}
	count := uint64(len(commit.children))
	if count > MaxDirectoryEntries || commit.omittedCount > MaxDirectoryEntries-count {
		return fmt.Errorf("%w: directory %x exceeds its entry limit", ErrPageLimit, directoryID)
	}
	if synthetic {
		if count == 0 || count > MaxSelectedRoots || commit.omittedCount != 0 {
			return fmt.Errorf("catalog synthetic root must contain 1..%d selected roots without omissions", MaxSelectedRoots)
		}
	}
	return nil
}

const (
	// Bounds cover canonical entry fields and wire envelope, leaving headroom for
	// one maximum-sized next name so encoded pages never exceed MaxCatalogPayloadBytes.
	pageEnvelopeBudgetBytes  = 512
	entryEncodingBudgetBytes = 128
)

// pageEncodingReservationBytes bounds worst-case in-memory wire encoding for a
// single page from its actual entries so empty or small directories do not
// require a full 60 KiB reservation when remaining budget is tight.
func pageEncodingReservationBytes(entries []Entry) uint64 {
	wireBytes := uint64(pageEnvelopeBudgetBytes)
	for _, entry := range entries {
		wireBytes += uint64(len(entry.Name()) + entryEncodingBudgetBytes)
		if wireBytes >= MaxCatalogPayloadBytes {
			wireBytes = MaxCatalogPayloadBytes
			break
		}
	}
	return uint64(CatalogPageMemoryOverhead) + wireBytes
}

func (s *CatalogStore) commitDirectoryLocked(
	ctx context.Context,
	commit DirectoryCommit,
	sessionBudget *BudgetAccount,
	meter *attemptResourceMeter,
	childrenPreCharged bool,
	owner *scanAttempt,
) (CommittedDirectory, *BudgetReservation, error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, nil, err
	}
	directoryID, _ := commit.directory.DirectoryID()

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return CommittedDirectory{}, nil, ErrCatalogClosed
	}
	if owner != nil && s.attempts[directoryID] != owner {
		s.mu.RUnlock()
		return CommittedDirectory{}, nil, context.Canceled
	}
	if existing, found := s.directories[directoryID]; found {
		validated, err := s.validateExistingCommitLocked(commit, existing)
		s.mu.RUnlock()
		return validated, nil, err
	}
	dirExists := false
	if existingDir, found := s.nodes[commit.directory.NodeID()]; found {
		if existingDir != commit.directory {
			s.mu.RUnlock()
			return CommittedDirectory{}, nil, ErrGenerationConflict
		}
		dirExists = true
	}
	s.mu.RUnlock()

	var dirNodeBytes uint64
	if !dirExists {
		dirNodeBytes = commit.directory.EstimatedMemoryBytes()
	}
	var childrenBytes uint64
	for _, child := range commit.children {
		childrenBytes += child.EstimatedMemoryBytes()
	}
	// Reserve entry slots and directory/node memory before encoding pages so
	// direct commits fail before allocating page payloads and incremental page
	// charges transfer directly into the retained store reservation.
	upfrontMemory := memoryDirectoryOverhead + dirNodeBytes
	if !childrenPreCharged {
		upfrontMemory += childrenBytes
	}
	if err := meter.Consume(ResourceUsage{
		Entries:     uint64(len(commit.children)),
		MemoryBytes: upfrontMemory,
	}); err != nil {
		return CommittedDirectory{}, nil, err
	}

	pages, committed, pageBytes, err := s.buildDirectoryPages(ctx, commit, meter)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}

	retainedUsage := ResourceUsage{
		Entries:     uint64(len(commit.children)),
		MemoryBytes: memoryDirectoryOverhead + dirNodeBytes + childrenBytes + pageBytes,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CommittedDirectory{}, nil, ErrCatalogClosed
	}
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, nil, err
	}
	if owner != nil && s.attempts[directoryID] != owner {
		return CommittedDirectory{}, nil, context.Canceled
	}
	if existingDir, found := s.nodes[commit.directory.NodeID()]; found && existingDir != commit.directory {
		return CommittedDirectory{}, nil, ErrGenerationConflict
	}
	for _, child := range commit.children {
		if _, found := s.nodes[child.NodeID()]; found {
			return CommittedDirectory{}, nil, ErrGenerationConflict
		}
	}
	retained, err := meter.retain(retainedUsage, sessionBudget)
	if err != nil {
		return CommittedDirectory{}, nil, err
	}
	if !dirExists {
		s.nodes[commit.directory.NodeID()] = commit.directory
	}
	for _, child := range commit.children {
		s.nodes[child.NodeID()] = child
	}
	s.directories[directoryID] = committedDirectoryState{
		committed: committed,
		pages:     pages,
	}
	s.held = append(s.held, retained)
	if owner != nil {
		// Retire the in-flight scan slot atomically with directory publication so a
		// post-commit panic or late cancellation cannot attach a failure to it.
		delete(s.attempts, directoryID)
	}
	return committed, retained, nil
}

func (s *CatalogStore) validateExistingCommitLocked(
	commit DirectoryCommit,
	existing committedDirectoryState,
) (CommittedDirectory, error) {
	directoryID, _ := commit.directory.DirectoryID()
	if existing.committed.ShareInstance() != s.shareInstance ||
		existing.committed.DirectoryID() != directoryID ||
		existing.committed.Generation() != commit.generation ||
		existing.committed.EntryCount() != uint64(len(commit.children)) ||
		existing.committed.OmittedCount() != commit.omittedCount {
		return CommittedDirectory{}, ErrGenerationConflict
	}
	storedDir, found := s.nodes[commit.directory.NodeID()]
	if !found || storedDir != commit.directory {
		return CommittedDirectory{}, ErrGenerationConflict
	}
	var cursor int
	for _, payload := range existing.pages {
		page, err := payload.decode()
		if err != nil {
			return CommittedDirectory{}, err
		}
		for _, entry := range page.entries {
			if cursor >= len(commit.children) {
				return CommittedDirectory{}, ErrGenerationConflict
			}
			child := commit.children[cursor]
			cursor++
			if !child.MatchesEntry(entry) {
				return CommittedDirectory{}, ErrGenerationConflict
			}
			storedChild, ok := s.nodes[child.NodeID()]
			if !ok || storedChild != child {
				return CommittedDirectory{}, ErrGenerationConflict
			}
		}
	}
	if cursor != len(commit.children) {
		return CommittedDirectory{}, ErrGenerationConflict
	}
	return existing.committed, nil
}

func (s *CatalogStore) buildDirectoryPages(
	ctx context.Context,
	commit DirectoryCommit,
	meter *attemptResourceMeter,
) ([]PagePayload, CommittedDirectory, uint64, error) {
	directoryID, _ := commit.directory.DirectoryID()
	var validator DirectoryGenerationValidator

	if len(commit.children) == 0 {
		payload, _, pageBytes, err := encodeAndAcceptPage(
			CatalogPageSpec{
				ShareInstance: s.shareInstance,
				DirectoryID:   directoryID,
				Generation:    commit.generation,
				Terminal:      true,
				OmittedCount:  commit.omittedCount,
			},
			&validator,
			meter,
		)
		if err != nil {
			return nil, CommittedDirectory{}, 0, err
		}
		committed, err := validator.Finish()
		if err != nil {
			return nil, CommittedDirectory{}, 0, err
		}
		return []PagePayload{payload}, committed, pageBytes, nil
	}

	var (
		pages     []PagePayload
		previous  PageCommitment
		lastName  string
		pageBytes uint64
		consumed  int
		pageIndex uint32
	)
	for consumed < len(commit.children) {
		if err := ctx.Err(); err != nil {
			return nil, CommittedDirectory{}, 0, err
		}
		entries, err := collectPageEntries(
			directoryID, commit.children, &consumed, &lastName,
		)
		if err != nil {
			return nil, CommittedDirectory{}, 0, err
		}
		terminal := consumed == len(commit.children)
		omitted := uint64(0)
		if terminal {
			omitted = commit.omittedCount
		}
		payload, commitment, retainedBytes, err := encodeAndAcceptPage(
			CatalogPageSpec{
				ShareInstance: s.shareInstance,
				DirectoryID:   directoryID,
				Generation:    commit.generation,
				PageIndex:     pageIndex,
				Previous:      previous,
				Entries:       entries,
				Terminal:      terminal,
				OmittedCount:  omitted,
			},
			&validator,
			meter,
		)
		if err != nil {
			return nil, CommittedDirectory{}, 0, err
		}
		pages = append(pages, payload)
		pageBytes += retainedBytes
		previous = commitment
		pageIndex++
	}
	committed, err := validator.Finish()
	if err != nil {
		return nil, CommittedDirectory{}, 0, err
	}
	return pages, committed, pageBytes, nil
}

// encodeAndAcceptPage reserves worst-case page encoding memory on the commit
// meter before allocating wire bytes, then shrinks to the retained payload size
// once the decoded CatalogPage is validated and discarded.
func encodeAndAcceptPage(
	spec CatalogPageSpec,
	validator *DirectoryGenerationValidator,
	meter *attemptResourceMeter,
) (PagePayload, PageCommitment, uint64, error) {
	reservation := pageEncodingReservationBytes(spec.Entries)
	if err := meter.Consume(ResourceUsage{MemoryBytes: reservation}); err != nil {
		return PagePayload{}, PageCommitment{}, 0, err
	}
	page, payload, err := NewCatalogPage(spec)
	if err != nil {
		_ = meter.Release(ResourceUsage{MemoryBytes: reservation})
		return PagePayload{}, PageCommitment{}, 0, err
	}
	if err := validator.AcceptPage(page); err != nil {
		_ = meter.Release(ResourceUsage{MemoryBytes: reservation})
		return PagePayload{}, PageCommitment{}, 0, err
	}
	retainedBytes := memoryPageOverhead + uint64(len(payload.encoded))
	if reservation > retainedBytes {
		if err := meter.Release(ResourceUsage{MemoryBytes: reservation - retainedBytes}); err != nil {
			return PagePayload{}, PageCommitment{}, 0, err
		}
	}
	return payload, page.Commitment(), retainedBytes, nil
}

func collectPageEntries(
	parent DirectoryID,
	children []NodeRecord,
	consumed *int,
	lastName *string,
) ([]Entry, error) {
	remaining := len(children) - *consumed
	limit := min(MaxCatalogPageEntries, remaining)
	entries := make([]Entry, 0, limit)
	remainingBytes := MaxCatalogPayloadBytes - pageEnvelopeBudgetBytes
	for range limit {
		record := children[*consumed]
		if !record.valid() || record.Parent() != parent {
			return nil, errors.New("catalog child record is invalid or has another parent")
		}
		entry := record.Entry()
		if *consumed > 0 && *lastName >= entry.Name() {
			return nil, ErrPageSequence
		}
		entries = append(entries, entry)
		*lastName = entry.Name()
		*consumed++
		remainingBytes -= len(entry.Name()) + entryEncodingBudgetBytes
		if remainingBytes < MaxNameBytes+entryEncodingBudgetBytes {
			break
		}
	}
	return entries, nil
}
