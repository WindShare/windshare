package catalog

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// ScanAttemptLedgerBytes conservatively accounts for both attempt and
// generation replay guards retained for the share lifetime.
const ScanAttemptLedgerBytes = 96

type CatalogStore struct {
	shareInstance ShareInstance
	backend       CatalogBackend
	processBudget *BudgetAccount
	shareBudget   *BudgetAccount
	pageSealer    PageSealer
	failureSealer FailureSealer
	spillFactory  SpillFactory
	sortRunBytes  uint64
	clock         Clock
	attemptIDs    ScanAttemptIDGenerator
	generations   DirectoryGenerationGenerator

	commitMu             sync.Mutex
	mu                   sync.Mutex
	closed               bool
	held                 []*BudgetReservation
	recoveredReservation *BudgetReservation
	committedRoot        *committedRootState
	attempts             map[DirectoryID]*scanAttempt
	usedAttempts         map[scanAttemptIdentity]struct{}
	usedGenerations      map[DirectoryGeneration]struct{}
	scanWG               sync.WaitGroup
}

type scanAttemptIdentity struct {
	directory DirectoryID
	attempt   ScanAttemptID
}

func NewCatalogStore(config StoreConfig) (result *CatalogStore, resultErr error) {
	if config.FailureSealer == nil {
		config.FailureSealer, _ = config.PageSealer.(FailureSealer)
	}
	if config.ShareInstance.IsZero() || config.Backend == nil || config.ProcessBudget == nil ||
		config.ShareBudget == nil || config.PageSealer == nil || config.FailureSealer == nil {
		return nil, errors.New("catalog store requires share identity, backend, budgets, and object sealers")
	}
	if config.ProcessBudget == config.ShareBudget {
		return nil, errors.New("catalog store process and share budgets must be distinct")
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.AttemptIDs == nil {
		config.AttemptIDs = randomCatalogIdentities{}
	}
	if config.Generations == nil {
		config.Generations = randomCatalogIdentities{}
	}
	cleanupDefaultSpill := false
	if config.SpillFactory == nil {
		factory, err := defaultCatalogSpillFactory(config.Backend)
		if err != nil {
			return nil, err
		}
		config.SpillFactory = factory
		cleanupDefaultSpill = true
	}
	defer func() {
		if cleanupDefaultSpill {
			if lifecycle, ok := config.SpillFactory.(SpillLifecycle); ok {
				resultErr = errors.Join(resultErr, lifecycle.Destroy(config.ShareInstance))
			}
		}
	}()
	if config.SortRunBytes == 0 {
		config.SortRunBytes = DefaultSortRunMemoryBytes
	}
	if lifecycle, ok := config.SpillFactory.(SpillLifecycle); ok {
		if err := lifecycle.Recover(context.Background(), config.ShareInstance); err != nil {
			return nil, fmt.Errorf("recover catalog spill storage: %w", err)
		}
	}
	recovered, err := config.Backend.Recover(context.Background())
	if err != nil {
		return nil, fmt.Errorf("recover catalog backend: %w", err)
	}
	var recoveredReservation *BudgetReservation
	if recovered != (ResourceUsage{}) {
		recoveredReservation, err = reserveAccounts(
			[]*BudgetAccount{config.ProcessBudget, config.ShareBudget}, recovered,
		)
		if err != nil {
			return nil, fmt.Errorf("admit recovered catalog state: %w", err)
		}
	}
	store := &CatalogStore{
		shareInstance: config.ShareInstance, backend: config.Backend,
		processBudget: config.ProcessBudget, shareBudget: config.ShareBudget,
		pageSealer: config.PageSealer, failureSealer: config.FailureSealer,
		spillFactory: config.SpillFactory, sortRunBytes: config.SortRunBytes,
		clock: config.Clock, attemptIDs: config.AttemptIDs, generations: config.Generations,
		attempts: make(map[DirectoryID]*scanAttempt), usedAttempts: make(map[scanAttemptIdentity]struct{}),
		usedGenerations: make(map[DirectoryGeneration]struct{}), recoveredReservation: recoveredReservation,
	}
	if recoveredReservation != nil {
		store.held = append(store.held, recoveredReservation)
	}
	if err := store.restoreFailureAuthority(context.Background()); err != nil {
		if recoveredReservation != nil {
			recoveredReservation.Release()
		}
		return nil, fmt.Errorf("restore catalog failure authority: %w", err)
	}
	cleanupDefaultSpill = false
	return store, nil
}

func (s *CatalogStore) hierarchy(session *BudgetAccount) BudgetHierarchy {
	return BudgetHierarchy{Process: s.processBudget, Share: s.shareBudget, Session: session}
}

func (s *CatalogStore) Directory(ctx context.Context, directory DirectoryID) (CommittedDirectory, bool, error) {
	if s.isClosed() {
		return CommittedDirectory{}, false, ErrCatalogClosed
	}
	committed, found, err := s.backend.LoadDirectory(ctx, directory)
	if err == nil && found && (committed.ShareInstance() != s.shareInstance || committed.DirectoryID() != directory) {
		return CommittedDirectory{}, false, ErrCorruptCatalogStorage
	}
	return committed, found, err
}

func (s *CatalogStore) Page(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (CatalogPage, bool, error) {
	if s.isClosed() {
		return CatalogPage{}, false, ErrCatalogClosed
	}
	page, found, err := s.backend.LoadPage(ctx, directory, generation, index)
	if err == nil && found && (page.ShareInstance() != s.shareInstance || page.DirectoryID() != directory ||
		page.Generation() != generation || page.PageIndex() != index) {
		return CatalogPage{}, false, ErrCorruptCatalogStorage
	}
	return page, found, err
}

func (s *CatalogStore) PageObject(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (SealedPageObject, bool, error) {
	if s.isClosed() {
		return SealedPageObject{}, false, ErrCatalogClosed
	}
	object, found, err := s.backend.LoadPageObject(ctx, directory, generation, index)
	if err != nil || !found {
		return SealedPageObject{}, found, err
	}
	page, pageFound, err := s.backend.LoadPage(ctx, directory, generation, index)
	if err != nil {
		return SealedPageObject{}, false, err
	}
	if !pageFound || page.ShareInstance() != s.shareInstance || page.DirectoryID() != directory ||
		page.Generation() != generation || page.PageIndex() != index || object.Commitment() != page.Commitment() {
		return SealedPageObject{}, false, ErrCorruptCatalogStorage
	}
	return object, true, nil
}

func (s *CatalogStore) Node(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
	if s.isClosed() {
		return NodeRecord{}, false, ErrCatalogClosed
	}
	record, found, err := s.backend.LoadNode(ctx, id)
	if err == nil && found && record.NodeID() != id {
		return NodeRecord{}, false, ErrCorruptCatalogStorage
	}
	return record, found, err
}

func (s *CatalogStore) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *CatalogStore) Close() error {
	s.commitMu.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.commitMu.Unlock()
		return nil
	}
	s.closed = true
	held := s.held
	s.held = nil
	s.recoveredReservation = nil
	s.committedRoot = nil
	attempts := make([]*scanAttempt, 0, len(s.attempts))
	for _, attempt := range s.attempts {
		if !attempt.completed {
			attempts = append(attempts, attempt)
		}
	}
	s.attempts = nil
	s.usedAttempts = nil
	s.usedGenerations = nil
	s.mu.Unlock()
	s.commitMu.Unlock()
	for _, attempt := range attempts {
		attempt.cancel()
	}
	s.scanWG.Wait()
	var closeErr error
	if destroyable, ok := s.backend.(interface{ Destroy() error }); ok {
		closeErr = destroyable.Destroy()
	} else {
		closeErr = s.backend.Close()
	}
	for _, reservation := range held {
		reservation.Release()
	}
	if lifecycle, ok := s.spillFactory.(SpillLifecycle); ok {
		closeErr = errors.Join(closeErr, lifecycle.Destroy(s.shareInstance))
	}
	return closeErr
}

func (s *CatalogStore) ListChildren(ctx context.Context, directory DirectoryID, sessionBudget *BudgetAccount, options ScanOptions, scanner DirectoryScanner) (CommittedDirectory, error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, err
	}
	if directory.IsZero() || sessionBudget == nil || scanner == nil {
		return CommittedDirectory{}, errors.New("catalog listing requires directory, session budget, and scanner")
	}
	if committed, exists, err := s.Directory(ctx, directory); err != nil {
		return CommittedDirectory{}, err
	} else if exists {
		return committed, nil
	}
	record, exists, err := s.Node(ctx, directory.NodeID())
	if err != nil {
		return CommittedDirectory{}, err
	}
	if !exists {
		return CommittedDirectory{}, errors.New("catalog directory is not known by its parent generation")
	}
	if recordID, ok := record.DirectoryID(); !ok || recordID != directory {
		return CommittedDirectory{}, errors.New("catalog node is not the requested directory")
	}

	now := s.clock.Now()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return CommittedDirectory{}, ErrCatalogClosed
	}
	var previousAttempt ScanAttemptID
	if existing := s.attempts[directory]; existing != nil {
		if !existing.completed {
			existing.waiters++
			subscription := s.subscribeAttemptLocked(ctx, existing, options.Progress)
			s.mu.Unlock()
			return s.awaitAttempt(ctx, existing, subscription)
		}
		if !existing.retryable || !options.Retry || now.Before(existing.retryAt) {
			err := existing.err
			s.mu.Unlock()
			return CommittedDirectory{}, err
		}
		previousAttempt = existing.id
	}
	attempt, err := s.admitAttempt(directory, previousAttempt, sessionBudget)
	if err != nil {
		s.mu.Unlock()
		return CommittedDirectory{}, err
	}
	s.attempts[directory] = attempt
	subscription := s.subscribeAttemptLocked(ctx, attempt, options.Progress)
	s.scanWG.Add(1)
	s.mu.Unlock()
	go s.runAttempt(attempt, record, sessionBudget, scanner)
	return s.awaitAttempt(ctx, attempt, subscription)
}

func (s *CatalogStore) admitAttempt(directory DirectoryID, previous ScanAttemptID, session *BudgetAccount) (*scanAttempt, error) {
	admission, err := ReserveHierarchy(s.hierarchy(session), ResourceUsage{ActiveScans: 1})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*scanAttempt, error) {
		admission.Release()
		return nil, err
	}
	attemptID, err := s.attemptIDs.NewScanAttemptID()
	if err != nil || attemptID.IsZero() {
		if err == nil {
			err = errors.New("catalog scan attempt generator returned a zero identity")
		}
		return fail(err)
	}
	generation, err := s.generations.NewDirectoryGeneration()
	if err != nil || generation.IsZero() {
		if err == nil {
			err = errors.New("catalog generation generator returned a zero identity")
		}
		return fail(err)
	}
	identity := scanAttemptIdentity{directory: directory, attempt: attemptID}
	if _, exists := s.usedAttempts[identity]; exists {
		return fail(errors.New("catalog scan attempt generator reused an identity"))
	}
	if _, exists := s.usedGenerations[generation]; exists {
		return fail(errors.New("catalog generation generator reused an identity"))
	}
	sessionCheck, err := reserveAccounts([]*BudgetAccount{session}, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes})
	if err != nil {
		return fail(err)
	}
	sessionCheck.Release()
	ledger, err := reserveAccounts(
		[]*BudgetAccount{s.processBudget, s.shareBudget}, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes},
	)
	if err != nil {
		return fail(err)
	}
	resources, err := newAttemptResourceMeter(s.hierarchy(session))
	if err != nil {
		ledger.Release()
		return fail(err)
	}
	s.usedAttempts[identity] = struct{}{}
	s.usedGenerations[generation] = struct{}{}
	s.held = append(s.held, ledger)
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &scanAttempt{
		id: attemptID, generation: generation, directory: directory, previous: previous, done: make(chan struct{}),
		ctx: attemptContext, cancel: cancel, waiters: 1, admission: admission, resources: resources,
		observers: make(map[*scanProgressSubscription]struct{}),
	}
	return attempt, nil
}

func (s *CatalogStore) awaitAttempt(
	ctx context.Context,
	attempt *scanAttempt,
	subscription *scanProgressSubscription,
) (CommittedDirectory, error) {
	select {
	case <-attempt.done:
		s.mu.Lock()
		attempt.waiters--
		committed, err := attempt.committed, attempt.err
		s.detachProgressLocked(attempt, subscription, true, nil)
		s.mu.Unlock()
		return committed, errors.Join(err, finishScanProgressSubscription(subscription))
	case <-ctx.Done():
		s.mu.Lock()
		attempt.waiters--
		s.detachProgressLocked(attempt, subscription, false, ctx.Err())
		if !attempt.completed {
			if attempt.waiters == 0 {
				if s.attempts[attempt.directory] == attempt {
					delete(s.attempts, attempt.directory)
				}
				attempt.cancel()
			}
		}
		s.mu.Unlock()
		_ = finishScanProgressSubscription(subscription)
		return CommittedDirectory{}, ctx.Err()
	case <-scanProgressDone(subscription):
		progressErr := scanProgressCause(subscription)
		s.mu.Lock()
		attempt.waiters--
		s.detachProgressLocked(attempt, subscription, false, progressErr)
		if !attempt.completed && attempt.waiters == 0 {
			if s.attempts[attempt.directory] == attempt {
				delete(s.attempts, attempt.directory)
			}
			attempt.cancel()
		}
		s.mu.Unlock()
		_ = finishScanProgressSubscription(subscription)
		return CommittedDirectory{}, progressErr
	}
}

type scanWorkMeter struct {
	store     *CatalogStore
	resources *attemptResourceMeter
}

func (m scanWorkMeter) Consume(units uint64) error {
	if units == 0 {
		return nil
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	if m.store.closed {
		return ErrCatalogClosed
	}
	return m.resources.Consume(ResourceUsage{ScanWork: units})
}

type scanChildCollector struct {
	mu       sync.Mutex
	ctx      context.Context
	work     ScanWork
	sorter   *directorySorter
	count    uint64
	closed   bool
	progress func(uint64)
}

func (c *scanChildCollector) Add(ctx context.Context, child ScannedChild) error {
	if ctx == nil {
		return errors.New("catalog scan child sink requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrScanSinkClosed
	}
	bound := c.ctx
	if bound == nil {
		bound = ctx
	}
	if err := bound.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.count == MaxDirectoryEntries {
		c.mu.Unlock()
		return ErrPageLimit
	}
	if err := c.work.Consume(1); err != nil {
		c.mu.Unlock()
		return err
	}
	if err := c.sorter.Add(bound, child); err != nil {
		c.mu.Unlock()
		return err
	}
	c.count++
	count := c.count
	progress := c.progress
	c.mu.Unlock()
	if progress != nil {
		progress(count)
	}
	return nil
}

func (c *scanChildCollector) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *scanChildCollector) Count() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (s *CatalogStore) runAttempt(attempt *scanAttempt, directory NodeRecord, sessionBudget *BudgetAccount, scanner DirectoryScanner) {
	defer s.scanWG.Done()
	attemptContext := attempt.ctx
	var committed CommittedDirectory
	var resultErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = fmt.Errorf("catalog scanner panicked: %v\n%s", recovered, debug.Stack())
		}
		if resultErr != nil {
			s.mu.Lock()
			eligible := !s.closed && s.attempts[attempt.directory] == attempt
			s.mu.Unlock()
			if eligible {
				failure, err := s.persistAttemptFailure(attempt, resultErr, sessionBudget)
				if err == nil {
					attempt.failure = failure
					resultErr = failure
				} else {
					resultErr = errors.Join(resultErr, fmt.Errorf("persist catalog directory failure: %w", err))
				}
			}
		}
		attempt.admission.Release()
		attempt.resources.Close()
		s.completeAttempt(attempt, committed, resultErr)
	}()
	work := scanWorkMeter{store: s, resources: attempt.resources}
	if err := work.Consume(1); err != nil {
		resultErr = err
		return
	}
	workspace, err := s.spillFactory.NewWorkspace(attemptContext, SpillRequest{
		ShareInstance: s.shareInstance, AttemptID: attempt.id,
	})
	if err != nil {
		resultErr = err
		return
	}
	defer func() {
		resultErr = errors.Join(resultErr, workspace.Close())
	}()
	sorter, err := newDirectorySorter(
		attempt.directory, workspace, attempt.resources, s.hierarchy(sessionBudget), s.sortRunBytes,
	)
	if err != nil {
		resultErr = err
		return
	}
	defer sorter.Close()
	collector := &scanChildCollector{
		ctx: attemptContext, work: work, sorter: sorter,
		progress: func(count uint64) { s.publishAttemptProgress(attempt, count, false) },
	}
	defer collector.Close()
	result, err := scanner.ScanDirectory(attemptContext, ScanRequest{
		AttemptID: attempt.id, Generation: attempt.generation, Directory: directory,
		Work: work, Children: collector,
	})
	collector.Close()
	s.publishAttemptProgress(attempt, collector.Count(), true)
	if err != nil {
		resultErr = err
		return
	}
	source, err := sorter.Finish(attemptContext)
	if err != nil {
		resultErr = err
		return
	}
	commit := DirectoryCommit{
		directory: directory, generation: attempt.generation, children: source, omittedCount: result.OmittedCount,
	}
	if err := validateDirectoryCommit(s.shareInstance, commit, false); err != nil {
		_ = source.Release(attempt.resources)
		resultErr = err
		return
	}
	s.commitMu.Lock()
	committed, _, err = s.commitDirectoryLocked(attemptContext, commit, sessionBudget, attempt.resources)
	s.commitMu.Unlock()
	if err != nil {
		resultErr = err
	}
}

func (s *CatalogStore) completeAttempt(attempt *scanAttempt, committed CommittedDirectory, resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if attempt.completed {
		return
	}
	attempt.completed = true
	attempt.committed = committed
	if resultErr == nil {
		if s.attempts[attempt.directory] == attempt {
			delete(s.attempts, attempt.directory)
		}
	} else {
		s.completeFailedAttemptLocked(attempt, resultErr)
	}
	attempt.cancel()
	close(attempt.done)
}
