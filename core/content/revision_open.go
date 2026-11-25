package content

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/windshare/windshare/core/capacityledger"
	"github.com/windshare/windshare/core/catalog"
)

var errRetryOpenForWaitingSession = errors.New("retry coalesced open for waiting session")

type openAttempt struct {
	file           catalog.FileID
	done           chan struct{}
	cancel         context.CancelFunc
	waiters        int
	completed      bool
	ownerAbandoned bool
	ownerSession   *RevisionSession
	ownerLease     RevisionLease
	revision       *revisionState
	err            error
}

type openWorkResult struct {
	stable      StableFile
	descriptor  FileRevisionDescriptor
	identity    revisionIdentity
	leaseID     LeaseID
	reservation newRevisionReservation
	reserved    bool
	comparison  RevisionComparison
	metadata    SourceMetadata
	cause       RevisionTraceCause
	err         error
}

func (s *RevisionStore) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
	session *RevisionSession,
) (RevisionLease, error) {
	if err := ctx.Err(); err != nil {
		return RevisionLease{}, err
	}
	if file.IsZero() || session == nil {
		return RevisionLease{}, errors.New("open revision requires a file identity and revision session")
	}
	s.reap(s.clock.Now())

	reapedPool := false
	for {
		s.pool.mu.Lock()
		if s.closed {
			s.pool.mu.Unlock()
			return RevisionLease{}, ErrRevisionStoreClosed
		}
		if revision := s.activeResidentLocked(file); revision != nil {
			lease, retry, err := s.openResidentUnlock(ctx, revision, session, &reapedPool)
			if retry {
				continue
			}
			return lease, err
		}
		if attempt := s.opening[file]; attempt != nil {
			attempt.waiters++
			s.pool.mu.Unlock()
			lease, err := s.awaitOpen(ctx, attempt, false)
			if errors.Is(err, errRetryOpenForWaitingSession) {
				continue
			}
			return lease, err
		}
		attemptContext, cancel := context.WithCancel(s.closeContext)
		attempt := &openAttempt{
			file: file, done: make(chan struct{}), cancel: cancel, waiters: 1, ownerSession: session,
		}
		s.opening[file] = attempt
		s.openWG.Add(1)
		s.pool.mu.Unlock()
		go s.runOpen(attemptContext, attempt)
		return s.awaitOpen(ctx, attempt, true)
	}
}

func (s *RevisionStore) activeResidentLocked(file catalog.FileID) *revisionState {
	revision := s.revisions[file]
	if revision == nil || revision.lifecycle != revisionLifecycleActive || revision.closed || revision.closing {
		return nil
	}
	return revision
}

func (s *RevisionStore) openResidentUnlock(
	ctx context.Context,
	revision *revisionState,
	session *RevisionSession,
	reapedPool *bool,
) (RevisionLease, bool, error) {
	var effects reapEffects
	lease, trace, needPoolReap, err := s.openResidentLocked(revision, session, !*reapedPool)
	if needPoolReap {
		reaped := s.pool.reapAllStoresLocked(&effects)
		*reapedPool = true
		if reaped && (len(effects.cleanups) > 0 || s.activeResidentLocked(revision.descriptor.FileID()) != revision) {
			s.pool.mu.Unlock()
			effects.run()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return RevisionLease{}, false, ctxErr
			}
			return RevisionLease{}, true, nil
		}
		lease, trace, _, err = s.openResidentLocked(revision, session, false)
	}
	s.pool.mu.Unlock()
	effects.run()
	s.pool.emitTrace(trace)
	if err == nil {
		descriptor := lease.Descriptor()
		s.traceRevision(RevisionTraceStageActiveReuse, RevisionTraceCauseUnknown, descriptor.FileID(), descriptor.FileRevision())
	}
	return lease, false, err
}

// openResidentLocked admits a new lease on an already-open (active or idle
// grace-period) revision in a single critical section.
func (s *RevisionStore) openResidentLocked(
	revision *revisionState,
	session *RevisionSession,
	allowPoolReap bool,
) (RevisionLease, *capacityTracePublication, bool, error) {
	pool := s.pool
	if pool.closed {
		return RevisionLease{}, nil, false, ErrHandlePoolClosed
	}
	if session == nil || session.store != s {
		return RevisionLease{}, nil, false, errors.New("revision session belongs to another store")
	}
	if session.closed {
		return RevisionLease{}, nil, false, ErrSessionClosed
	}
	revisionID := capacityRevisionID(revision.identity())
	leaseHold, ok := session.leaseLedger.Reserve(1)
	if !ok {
		scope := pool.leaseBlockScopeLocked(s, session)
		if allowPoolReap && scope == CapacityScopeProcess {
			return RevisionLease{}, nil, true, nil
		}
		busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceActiveLease, scope)
		return RevisionLease{}, trace, false, busy
	}
	var fileHold capacityledger.Hold
	if revision.sessionHandles[session] == nil {
		fileHold, ok = session.fileLedger.Reserve(1)
		if !ok {
			leaseHold.Release()
			busy, trace := pool.denyLocked(s, session, revisionID, CapacityResourceStableHandle, CapacityScopeSession)
			return RevisionLease{}, trace, false, busy
		}
	}
	leaseID, err := s.newLeaseIDLocked()
	if err != nil {
		leaseHold.Release()
		fileHold.Release()
		return RevisionLease{}, nil, false, err
	}
	pool.unlinkIdleLocked(revision)
	lease, err := s.publishLeaseLocked(revision, session, s.clock.Now(), leaseID, leaseHold, fileHold)
	if err != nil {
		leaseHold.Release()
		fileHold.Release()
		return RevisionLease{}, nil, false, err
	}
	decision := pool.nextDecisionIDLocked()
	trace := pool.makeTraceLocked(CapacityTraceAdmitted, decision, s, session, revisionID, nil)
	return lease, trace, false, nil
}

func (s *RevisionStore) awaitOpen(
	ctx context.Context,
	attempt *openAttempt,
	owner bool,
) (RevisionLease, error) {
	select {
	case <-attempt.done:
		s.pool.mu.Lock()
		attempt.waiters--
		err := attempt.err
		lease := attempt.ownerLease
		s.pool.mu.Unlock()
		if err != nil {
			if !owner && errors.Is(err, ErrSessionClosed) {
				// Coalescing is store-scoped, so one departed session must not make
				// another session inherit an otherwise healthy open attempt's failure.
				return RevisionLease{}, errRetryOpenForWaitingSession
			}
			return RevisionLease{}, err
		}
		if owner && !lease.ID().IsZero() {
			return lease, nil
		}
		// A waiter never inherits another session's lease; re-enter under its own session.
		return RevisionLease{}, errRetryOpenForWaitingSession
	case <-ctx.Done():
		var abandonedLease LeaseID
		s.pool.mu.Lock()
		attempt.waiters--
		if owner {
			attempt.ownerAbandoned = true
			abandonedLease = attempt.ownerLease.ID()
		}
		if !attempt.completed && attempt.waiters == 0 {
			if s.opening[attempt.file] == attempt {
				delete(s.opening, attempt.file)
			}
			attempt.cancel()
		}
		s.pool.mu.Unlock()
		if !abandonedLease.IsZero() {
			_ = s.EndLease(abandonedLease, LeaseUndelivered)
		}
		return RevisionLease{}, ctx.Err()
	}
}

func (s *RevisionStore) runOpen(ctx context.Context, attempt *openAttempt) {
	defer s.openWG.Done()
	result := openWorkResult{}
	defer func() {
		if recovered := recover(); recovered != nil {
			result.comparison = RevisionComparisonUnavailable
			result.cause = RevisionTraceCausePanic
			result.err = fmt.Errorf("revision source panicked: %v\n%s", recovered, debug.Stack())
		}
		s.completeOpen(attempt, result)
	}()
	record, ready := s.resolveOpenIdentity(ctx, attempt, &result)
	if !ready {
		return
	}

	current, evicted, trace, err := s.reserveForOpen(attempt, &result)
	s.pool.emitTrace(trace)
	// Always finish closing a detached victim even if the requester canceled or
	// admission failed, so victimStore.Close() joins cleanly and no FD leaks.
	if evicted != nil {
		_ = evicted.close()
	}
	if !current {
		result.err = context.Canceled
		return
	}
	if err != nil {
		result.comparison = RevisionComparisonUnavailable
		result.cause = RevisionTraceCauseCapacity
		var busy *CapacityBusyError
		if !errors.As(err, &busy) && !errors.Is(err, ErrSessionClosed) && !errors.Is(err, ErrHandlePoolClosed) {
			result.cause = RevisionTraceCauseCatalog
		}
		result.err = err
		return
	}
	if err := ctx.Err(); err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCapacity, err
		return
	}

	result.stable, err = s.source.OpenStable(ctx, record)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonOf(err), RevisionTraceCauseSourceOpen, err
		return
	}
	if err := result.stable.Verify(ctx); err != nil {
		result.comparison, result.cause = RevisionComparisonOf(err), RevisionTraceCauseVerification
		if result.comparison == RevisionComparisonMismatch {
			result.err = errors.Join(ErrRevisionStale, err)
		} else {
			result.err = fmt.Errorf("verify stable file before revision publication: %w", err)
		}
		return
	}
	expectedSize := record.Entry().ExpectedSize()
	if result.stable.ExactSize() != expectedSize {
		result.comparison, result.cause, result.err = RevisionComparisonMismatch, RevisionTraceCauseGeometry, ErrRevisionStale
		return
	}
	if result.stable.ModifiedTime() != record.Entry().ModifiedTime() {
		result.comparison, result.cause, result.err = RevisionComparisonMismatch, RevisionTraceCauseModifiedTime, ErrRevisionStale
		return
	}
	geometry, err := NewFileGeometry(expectedSize, s.chunkSize)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseGeometry, err
		return
	}
	result.descriptor, err = NewFileRevisionDescriptor(
		s.shareInstance, attempt.file, result.identity.revision, geometry, record.Entry().ModifiedTime(),
	)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseGeometry, err
		return
	}
	result.descriptor = BindRevisionMetadata(result.descriptor, result.metadata)
	result.comparison = RevisionComparisonMetadataAgreement
}

func (s *RevisionStore) reserveForOpen(
	attempt *openAttempt,
	result *openWorkResult,
) (bool, *evictedRevision, *capacityTracePublication, error) {
	reapedPool := false
	for {
		s.pool.mu.Lock()
		current := s.opening[attempt.file] == attempt && !s.closed
		if !current {
			s.pool.mu.Unlock()
			return false, nil, nil, nil
		}
		if result.identity.revision.IsZero() {
			revisionID, err := s.newFileRevisionLocked(attempt.file)
			if err != nil {
				s.pool.mu.Unlock()
				return true, nil, nil, err
			}
			result.identity = revisionIdentity{file: attempt.file, revision: revisionID}
		}
		if result.leaseID.IsZero() {
			leaseID, err := s.newLeaseIDLocked()
			if err != nil {
				s.pool.mu.Unlock()
				return true, nil, nil, err
			}
			result.leaseID = leaseID
		}
		reservation, evicted, trace, needPoolReap, err := s.reserveNewRevisionLocked(attempt.ownerSession, result.identity, !reapedPool)
		if needPoolReap {
			var effects reapEffects
			reaped := s.pool.reapAllStoresLocked(&effects)
			reapedPool = true
			if reaped && len(effects.cleanups) > 0 {
				// Detached grace-expired StableFiles release handleHold only after
				// Close returns, so run them outside pool.mu before retrying.
				s.pool.mu.Unlock()
				effects.run()
				continue
			}
			reservation, evicted, trace, _, err = s.reserveNewRevisionLocked(attempt.ownerSession, result.identity, false)
			if err == nil {
				result.reservation = reservation
				result.reserved = true
			}
			s.pool.mu.Unlock()
			effects.run()
			return true, evicted, trace, err
		}
		if err == nil {
			result.reservation = reservation
			result.reserved = true
		}
		s.pool.mu.Unlock()
		return true, evicted, trace, err
	}
}

func (s *RevisionStore) resolveOpenIdentity(
	ctx context.Context,
	attempt *openAttempt,
	result *openWorkResult,
) (catalog.NodeRecord, bool) {
	record, exists, err := s.catalog.Node(ctx, attempt.file.NodeID())
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return catalog.NodeRecord{}, false
	}
	recordFile, isFile := record.FileID()
	if !exists || !isFile || recordFile != attempt.file {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, ErrRevisionNotFound
		return catalog.NodeRecord{}, false
	}
	evidence, err := NewRevisionEvidence(
		s.shareInstance, attempt.file, record.SourceIdentity(), record.VersionCandidate(),
		record.Entry().ExpectedSize(), record.Entry().ModifiedTime(), s.chunkSize,
	)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return catalog.NodeRecord{}, false
	}
	result.metadata = sourceMetadata(evidence)
	return record, true
}

func (s *RevisionStore) completeOpen(attempt *openAttempt, result openWorkResult) {
	s.pool.mu.Lock()
	if attempt.completed {
		s.pool.mu.Unlock()
		_ = closeStableFile(result.stable)
		if result.reserved {
			s.pool.mu.Lock()
			result.reservation.releaseLocked(s.pool)
			s.pool.mu.Unlock()
		}
		return
	}
	attempt.completed = true
	current := s.opening[attempt.file] == attempt
	if result.err == nil && result.stable != nil && current && !s.closed && s.revisions[attempt.file] != nil {
		result.err = errors.New("revision source raced an existing file revision")
	}
	if result.err == nil && attempt.ownerSession.closed {
		result.err = ErrSessionClosed
	}
	if result.err == nil && (!current || s.closed || result.stable == nil) {
		result.err = context.Canceled
	}
	if result.err == nil {
		revision := &revisionState{
			store:          s,
			descriptor:     result.descriptor,
			source:         result.stable,
			handleHold:     result.reservation.handleHold,
			leases:         make(map[LeaseID]*leaseState),
			sessionHandles: make(map[*RevisionSession]*sessionHandleState),
			lifecycle:      revisionLifecycleActive,
		}
		lease, publishErr := s.publishLeaseLocked(
			revision, attempt.ownerSession, s.clock.Now(), result.leaseID,
			result.reservation.leaseHold, result.reservation.fileHold,
		)
		if publishErr == nil {
			s.revisions[attempt.file] = revision
			attempt.revision = revision
			attempt.ownerLease = lease
			result.stable = nil
			result.reserved = false
			attempt.ownerSession.pending--
			s.pool.cond.Broadcast()
		} else {
			result.err = publishErr
		}
	}
	var toClose StableFile
	if result.err != nil {
		toClose = result.stable
		result.stable = nil
		if toClose == nil && result.reserved {
			result.reservation.releaseLocked(s.pool)
			result.reserved = false
		}
	}
	s.pool.mu.Unlock()

	if toClose != nil {
		// Close the file before releasing handleHold so live OS handles never exceed limits.
		result.err = errors.Join(result.err, closeStableFile(toClose))
		if result.reserved {
			s.pool.mu.Lock()
			result.reservation.releaseLocked(s.pool)
			result.reserved = false
			s.pool.mu.Unlock()
		}
	}

	s.pool.mu.Lock()
	attempt.err = result.err
	if s.opening[attempt.file] == attempt {
		delete(s.opening, attempt.file)
	}
	abandonedLease := LeaseID{}
	if attempt.ownerAbandoned {
		abandonedLease = attempt.ownerLease.ID()
	}
	s.pool.mu.Unlock()
	if !abandonedLease.IsZero() {
		_ = s.EndLease(abandonedLease, LeaseUndelivered)
	}
	s.traceOpenCompletion(result, attempt)
	close(attempt.done)
}

func (s *RevisionStore) traceOpenCompletion(result openWorkResult, attempt *openAttempt) {
	switch result.comparison {
	case RevisionComparisonMismatch:
		s.traceRevision(RevisionTraceStageOpenRejected, result.cause, result.identity.file, result.identity.revision)
		return
	case RevisionComparisonUnavailable:
		s.traceRevision(RevisionTraceStageUnavailableRetry, result.cause, attempt.file, result.identity.revision)
		return
	case RevisionComparisonMetadataAgreement, RevisionComparisonUnknown:
	default:
		return
	}
	if attempt.revision != nil {
		s.traceRevision(RevisionTraceStageOpenHandleBound, RevisionTraceCauseUnknown, result.identity.file, result.identity.revision)
	}
}
