package catalog

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

var (
	ErrCatalogClosed      = errors.New("catalog store is closed")
	ErrGenerationConflict = errors.New("catalog directory generation conflicts with committed state")
)

const (
	// ScanAttemptLedgerBytes conservatively accounts for both attempt and
	// generation replay guards retained for the share lifetime.
	ScanAttemptLedgerBytes  = 96
	memoryDirectoryOverhead = uint64(128)
	memoryPageOverhead      = uint64(128)
	memoryFailureOverhead   = uint64(128)
)

type committedDirectoryState struct {
	committed CommittedDirectory
	pages     []PagePayload
}

type committedFailure struct {
	record  DirectoryFailureRecord
	payload FailurePayload
}

type CatalogStore struct {
	shareInstance  ShareInstance
	processBudget  *BudgetAccount
	shareBudget    *BudgetAccount
	failureEncoder FailureEncoder
	clock          Clock
	attemptIDs     ScanAttemptIDGenerator
	generations    DirectoryGenerationGenerator

	commitMu        sync.Mutex
	mu              sync.RWMutex
	closed          bool
	held            []*BudgetReservation
	committedRoot   *committedRootState
	directories     map[DirectoryID]committedDirectoryState
	nodes           map[NodeID]NodeRecord
	failures        map[DirectoryID]map[ScanAttemptID]committedFailure
	attempts        map[DirectoryID]*scanAttempt
	usedAttempts    map[scanAttemptIdentity]struct{}
	usedGenerations map[DirectoryGeneration]struct{}
	activeScans     int
	scanSequence    uint64
	exclusiveRetry  bool
	retryQueue      []*scanAttempt
	scanTurnWake    chan struct{}
	scanWG          sync.WaitGroup
}

type scanAttemptIdentity struct {
	directory DirectoryID
	attempt   ScanAttemptID
}

func NewCatalogStore(config StoreConfig) (*CatalogStore, error) {
	if config.ShareInstance.IsZero() || config.ProcessBudget == nil ||
		config.ShareBudget == nil || config.FailureEncoder == nil {
		return nil, errors.New("catalog store requires share identity, budgets, and failure sealer")
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
	return &CatalogStore{
		shareInstance:   config.ShareInstance,
		processBudget:   config.ProcessBudget,
		shareBudget:     config.ShareBudget,
		failureEncoder:  config.FailureEncoder,
		clock:           config.Clock,
		attemptIDs:      config.AttemptIDs,
		generations:     config.Generations,
		directories:     make(map[DirectoryID]committedDirectoryState),
		nodes:           make(map[NodeID]NodeRecord),
		failures:        make(map[DirectoryID]map[ScanAttemptID]committedFailure),
		attempts:        make(map[DirectoryID]*scanAttempt),
		usedAttempts:    make(map[scanAttemptIdentity]struct{}),
		usedGenerations: make(map[DirectoryGeneration]struct{}),
	}, nil
}

func (s *CatalogStore) hierarchy(session *BudgetAccount) BudgetHierarchy {
	return BudgetHierarchy{Process: s.processBudget, Share: s.shareBudget, Session: session}
}

func (s *CatalogStore) Directory(ctx context.Context, directory DirectoryID) (CommittedDirectory, bool, error) {
	if err := ctx.Err(); err != nil {
		return CommittedDirectory{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return CommittedDirectory{}, false, ErrCatalogClosed
	}
	state, found := s.directories[directory]
	return state.committed, found, nil
}

func (s *CatalogStore) Page(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (CatalogPage, bool, error) {
	payload, found, err := s.PagePayload(ctx, directory, generation, index)
	if err != nil || !found {
		return CatalogPage{}, false, err
	}
	page, err := payload.decode()
	return page, err == nil, err
}

func (s *CatalogStore) PagePayload(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (PagePayload, bool, error) {
	if err := ctx.Err(); err != nil {
		return PagePayload{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return PagePayload{}, false, ErrCatalogClosed
	}
	state, found := s.directories[directory]
	if !found || state.committed.generation != generation || uint64(index) >= uint64(len(state.pages)) {
		return PagePayload{}, false, nil
	}
	payload := state.pages[index]
	address := PageAddress{ShareInstance: s.shareInstance, DirectoryID: directory, Generation: generation, PageIndex: index}
	if payload.IsZero() || payload.Address() != address {
		return PagePayload{}, false, ErrInvalidPagePayload
	}
	return payload, true, nil
}

func (s *CatalogStore) Node(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return NodeRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return NodeRecord{}, false, ErrCatalogClosed
	}
	record, found := s.nodes[id]
	return record, found, nil
}

func (s *CatalogStore) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.committedRoot = nil
	attempts := make([]*scanAttempt, 0, len(s.attempts))
	for _, attempt := range s.attempts {
		if !attempt.completed {
			attempts = append(attempts, attempt)
		}
	}
	s.directories = nil
	s.nodes = nil
	s.failures = nil
	s.attempts = nil
	s.usedAttempts = nil
	s.usedGenerations = nil
	s.retryQueue = nil
	s.broadcastScanTurnLocked()
	s.mu.Unlock()
	s.commitMu.Unlock()
	for _, attempt := range attempts {
		attempt.cancel()
	}
	s.scanWG.Wait()
	for _, reservation := range held {
		reservation.Release()
	}
	return nil
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
	if state, exists := s.directories[directory]; exists {
		s.mu.Unlock()
		return state.committed, nil
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
		return fail(s.classifyScanBudgetError(err, s.activeScans == 0))
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
		if !attempt.completed && attempt.waiters == 0 {
			if s.attempts[attempt.directory] == attempt {
				delete(s.attempts, attempt.directory)
			}
			attempt.cancel()
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

func (s *CatalogStore) runAttempt(attempt *scanAttempt, directory NodeRecord, sessionBudget *BudgetAccount, scanner DirectoryScanner) {
	defer s.scanWG.Done()
	attemptContext := attempt.ctx
	var committed CommittedDirectory
	var resultErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			resultErr = fmt.Errorf("catalog scanner panicked: %v\n%s", recovered, debug.Stack())
		}
		if resultErr == nil {
			s.completeSucceededAttempt(attempt, committed)
			return
		}
		var (
			prepared   *preparedFailure
			prepareErr error
		)
		s.mu.RLock()
		eligible := !s.closed && s.attempts[attempt.directory] == attempt
		s.mu.RUnlock()
		if eligible {
			// Release transient scan work and buffered child memory before reserving
			// space to encode and retain the failure payload.
			attempt.resources.reset()
			if encoded, err := s.prepareAttemptFailure(attempt, resultErr); err == nil {
				prepared = &encoded
			} else {
				prepareErr = err
			}
		}
		s.completeFailedAttempt(attempt, resultErr, prepared, prepareErr, sessionBudget)
	}()
	var overlapped bool
	exclusivePass := false
	committed, overlapped, resultErr = s.runParallelScanPass(attempt, directory, sessionBudget, scanner)
	if resultErr == nil || attemptContext.Err() != nil {
		return
	}
	if s.shouldRetryContendedBudget(resultErr, sessionBudget, overlapped) {
		exclusivePass = true
		committed, resultErr = s.runExclusiveRetryPass(attempt, directory, sessionBudget, scanner)
	}
	if resultErr != nil {
		resultErr = s.classifyScanBudgetError(resultErr, exclusivePass)
	}
}

func (s *CatalogStore) runParallelScanPass(
	attempt *scanAttempt,
	directory NodeRecord,
	sessionBudget *BudgetAccount,
	scanner DirectoryScanner,
) (committed CommittedDirectory, overlapped bool, resultErr error) {
	startSeq, hadPeer, err := s.enterParallelScan(attempt.ctx)
	if err != nil {
		return CommittedDirectory{}, false, err
	}
	var succeeded bool
	defer func() {
		// Release transient buffers before waking exclusive retry waiters so the
		// woken scan observes freed capacity immediately, including on panic.
		if !succeeded {
			attempt.resources.reset()
		}
		overlapped = s.leaveParallelScan(startSeq, hadPeer)
	}()
	committed, resultErr = s.executeScanPass(attempt, directory, sessionBudget, scanner)
	succeeded = resultErr == nil
	return committed, overlapped, resultErr
}

func (s *CatalogStore) runExclusiveRetryPass(
	attempt *scanAttempt,
	directory NodeRecord,
	sessionBudget *BudgetAccount,
	scanner DirectoryScanner,
) (CommittedDirectory, error) {
	if err := s.enterExclusiveRetry(attempt.ctx, attempt); err != nil {
		return CommittedDirectory{}, err
	}
	var succeeded bool
	defer func() {
		if !succeeded {
			attempt.resources.reset()
		}
		s.leaveExclusiveRetry()
	}()
	committed, err := s.executeScanPass(attempt, directory, sessionBudget, scanner)
	succeeded = err == nil
	return committed, err
}

func (s *CatalogStore) executeScanPass(
	attempt *scanAttempt,
	directory NodeRecord,
	sessionBudget *BudgetAccount,
	scanner DirectoryScanner,
) (CommittedDirectory, error) {
	attemptContext := attempt.ctx
	work := scanWorkMeter{store: s, resources: attempt.resources}
	if err := work.Consume(1); err != nil {
		return CommittedDirectory{}, err
	}
	sorter, err := newDirectorySorter(attempt.directory, attempt.resources)
	if err != nil {
		return CommittedDirectory{}, err
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
		return CommittedDirectory{}, err
	}
	sortResult, err := sorter.Finish(attemptContext)
	if err != nil {
		return CommittedDirectory{}, err
	}
	if result.OmittedCount > MaxDirectoryEntries || sortResult.omittedCount > MaxDirectoryEntries-result.OmittedCount {
		return CommittedDirectory{}, fmt.Errorf("%w: directory %x exceeds its entry limit", ErrPageLimit, attempt.directory)
	}
	commit := DirectoryCommit{
		directory:    directory,
		generation:   attempt.generation,
		children:     sortResult.records,
		omittedCount: result.OmittedCount + sortResult.omittedCount,
	}
	if err := validateDirectoryCommit(s.shareInstance, commit, false); err != nil {
		return CommittedDirectory{}, err
	}
	s.commitMu.Lock()
	committed, _, err := s.commitDirectoryLocked(attemptContext, commit, sessionBudget, attempt.resources, true, attempt)
	s.commitMu.Unlock()
	if err != nil {
		return CommittedDirectory{}, err
	}
	result.PublishOmissions(sortResult.omittedCount, sortResult.omittedSamples)
	return committed, nil
}

func (s *CatalogStore) scanTurnWakeLocked() <-chan struct{} {
	if s.scanTurnWake == nil {
		s.scanTurnWake = make(chan struct{})
	}
	return s.scanTurnWake
}

func (s *CatalogStore) broadcastScanTurnLocked() {
	if s.scanTurnWake != nil {
		close(s.scanTurnWake)
		s.scanTurnWake = nil
	}
}

func (s *CatalogStore) removeRetryWaiterLocked(target *scanAttempt) {
	for index, queued := range s.retryQueue {
		if queued != target {
			continue
		}
		copy(s.retryQueue[index:], s.retryQueue[index+1:])
		s.retryQueue[len(s.retryQueue)-1] = nil
		s.retryQueue = s.retryQueue[:len(s.retryQueue)-1]
		return
	}
}

func (s *CatalogStore) enterParallelScan(ctx context.Context) (uint64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, false, ErrCatalogClosed
		}
		// Keep the fast path uncontended unless a budget-contended scan is
		// waiting for or running its serialized retry turn.
		if !s.exclusiveRetry && len(s.retryQueue) == 0 {
			hadPeer := s.activeScans > 0
			s.activeScans++
			s.scanSequence++
			startSeq := s.scanSequence
			s.mu.Unlock()
			return startSeq, hadPeer, nil
		}
		wake := s.scanTurnWakeLocked()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-wake:
		}
	}
}

func (s *CatalogStore) leaveParallelScan(startSeq uint64, hadPeer bool) bool {
	s.mu.Lock()
	overlapped := hadPeer || s.scanSequence != startSeq || s.activeScans > 1
	s.activeScans--
	s.scanSequence++
	if s.activeScans == 0 {
		s.broadcastScanTurnLocked()
	}
	s.mu.Unlock()
	return overlapped
}

func (s *CatalogStore) enterExclusiveRetry(ctx context.Context, attempt *scanAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrCatalogClosed
	}
	s.retryQueue = append(s.retryQueue, attempt)
	for {
		if s.closed {
			s.removeRetryWaiterLocked(attempt)
			s.broadcastScanTurnLocked()
			s.mu.Unlock()
			return ErrCatalogClosed
		}
		if !s.exclusiveRetry && s.activeScans == 0 && len(s.retryQueue) > 0 && s.retryQueue[0] == attempt {
			s.removeRetryWaiterLocked(attempt)
			s.exclusiveRetry = true
			s.activeScans++
			s.scanSequence++
			s.mu.Unlock()
			return nil
		}
		wake := s.scanTurnWakeLocked()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.removeRetryWaiterLocked(attempt)
			s.broadcastScanTurnLocked()
			s.mu.Unlock()
			return ctx.Err()
		case <-wake:
			s.mu.Lock()
		}
	}
}

func (s *CatalogStore) leaveExclusiveRetry() {
	s.mu.Lock()
	s.activeScans--
	s.scanSequence++
	s.exclusiveRetry = false
	s.broadcastScanTurnLocked()
	s.mu.Unlock()
}

func (s *CatalogStore) shouldRetryContendedBudget(err error, sessionBudget *BudgetAccount, overlapped bool) bool {
	var budgetErr *budgetExceededError
	if !errors.As(err, &budgetErr) || !budgetErr.contendedForActiveScan() {
		return false
	}
	// Session contention belongs to concurrent scans on that session; share or
	// process contention can be resolved locally only when another scan ran in
	// this store during the failed pass.
	return budgetErr.account == sessionBudget || overlapped
}

func (s *CatalogStore) completeSucceededAttempt(attempt *scanAttempt, committed CommittedDirectory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if attempt.completed {
		return
	}
	attempt.completed = true
	attempt.committed = committed
	if s.attempts[attempt.directory] == attempt {
		delete(s.attempts, attempt.directory)
	}
	finishAttemptLocked(attempt)
}

func finishAttemptLocked(attempt *scanAttempt) {
	// Release active-scan admission and any unretained attempt resources before
	// closing attempt.done so returning callers observe zero leaked session budget.
	attempt.admission.Release()
	attempt.resources.Close()
	attempt.cancel()
	close(attempt.done)
}
