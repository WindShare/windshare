package content

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/windshare/windshare/core/catalog"
)

type openAttempt struct {
	file           catalog.FileID
	done           chan struct{}
	cancel         context.CancelFunc
	waiters        int
	completed      bool
	ownerAdmission *openAdmission
	revision       *revisionState
	err            error
}

type openWorkResult struct {
	revision   *revisionState
	identity   revisionIdentity
	comparison RevisionComparison
	cause      RevisionTraceCause
	rejected   bool
	err        error
}

func (s *RevisionStore) OpenRevision(ctx context.Context, file catalog.FileID, sessionQuota *QuotaAccount) (RevisionLease, error) {
	if err := ctx.Err(); err != nil {
		return RevisionLease{}, err
	}
	if file.IsZero() || sessionQuota == nil {
		return RevisionLease{}, errors.New("open revision requires a file identity and session quota")
	}
	s.reap(s.clock.Now())

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RevisionLease{}, ErrRevisionStoreClosed
	}
	if revision := s.revisions[file]; revision != nil && revision.lifecycle == revisionLifecycleActive && !revision.closed {
		lease, err := s.acquireLeaseLocked(revision, sessionQuota, s.clock.Now(), nil)
		descriptor := revision.descriptor
		s.mu.Unlock()
		if err == nil {
			s.traceRevision(RevisionTraceStageActiveReuse, RevisionTraceCauseUnknown, descriptor.FileID(), descriptor.FileRevision())
		}
		return lease, err
	}
	if s.revisionAdmissionStopped {
		s.mu.Unlock()
		s.traceRevision(RevisionTraceStageMetadataBudgetStop, RevisionTraceCauseMetadataBudget, file, FileRevision{})
		return RevisionLease{}, fmt.Errorf("%w: revision admission stopped", ErrQuotaExceeded)
	}
	if attempt := s.opening[file]; attempt != nil {
		attempt.waiters++
		s.mu.Unlock()
		return s.awaitOpen(ctx, attempt, sessionQuota, false)
	}
	admission, err := s.reserveOpenAdmissionLocked(sessionQuota)
	if err != nil {
		s.mu.Unlock()
		return RevisionLease{}, err
	}
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &openAttempt{file: file, done: make(chan struct{}), cancel: cancel, waiters: 1, ownerAdmission: admission}
	s.opening[file] = attempt
	s.openWG.Add(1)
	s.mu.Unlock()
	go s.runOpen(attemptContext, attempt)
	return s.awaitOpen(ctx, attempt, sessionQuota, true)
}

func (s *RevisionStore) awaitOpen(ctx context.Context, attempt *openAttempt, sessionQuota *QuotaAccount, ownsAdmission bool) (RevisionLease, error) {
	select {
	case <-attempt.done:
		s.mu.Lock()
		attempt.waiters--
		var admission *openAdmission
		if ownsAdmission {
			admission = attempt.ownerAdmission
			attempt.ownerAdmission = nil
			if s.opening[attempt.file] == attempt {
				delete(s.opening, attempt.file)
			}
		}
		if attempt.err != nil {
			err := attempt.err
			s.mu.Unlock()
			admission.release()
			return RevisionLease{}, err
		}
		if s.closed || attempt.revision == nil || attempt.revision.closed {
			s.mu.Unlock()
			admission.release()
			return RevisionLease{}, ErrRevisionStoreClosed
		}
		lease, err := s.acquireLeaseLocked(attempt.revision, sessionQuota, s.clock.Now(), admission)
		s.mu.Unlock()
		return lease, err
	case <-ctx.Done():
		s.mu.Lock()
		var admission *openAdmission
		if ownsAdmission {
			admission = attempt.ownerAdmission
			attempt.ownerAdmission = nil
			if attempt.completed && s.opening[attempt.file] == attempt {
				delete(s.opening, attempt.file)
			}
		}
		if !attempt.completed {
			attempt.waiters--
			if attempt.waiters == 0 {
				if s.opening[attempt.file] == attempt {
					delete(s.opening, attempt.file)
				}
				attempt.cancel()
			}
		}
		s.mu.Unlock()
		admission.release()
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
	record, exists, err := s.catalog.Node(ctx, attempt.file.NodeID())
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return
	}
	recordFile, isFile := record.FileID()
	if !exists || !isFile || recordFile != attempt.file {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, ErrRevisionNotFound
		return
	}
	evidence, err := NewRevisionEvidence(
		s.shareInstance, attempt.file, record.SourceIdentity(), record.VersionCandidate(),
		record.Entry().ExpectedSize(), record.Entry().ModifiedTime(), s.chunkSize,
	)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return
	}
	revisionID, err := s.revisionDeriver.DeriveRevision(evidence)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return
	}
	result.identity = revisionIdentity{file: attempt.file, revision: revisionID}
	s.mu.Lock()
	_, invalidated := s.invalidated[result.identity]
	admissionStopped := s.revisionAdmissionStopped
	s.mu.Unlock()
	if invalidated {
		result.rejected, result.cause, result.err = true, RevisionTraceCauseKnownInvalidation, ErrRevisionDrift
		return
	}
	if admissionStopped {
		result.rejected, result.cause = true, RevisionTraceCauseMetadataBudget
		result.err = fmt.Errorf("%w: revision admission stopped", ErrQuotaExceeded)
		return
	}

	// The physical source is share-scoped and survives ProtocolSession reconnects;
	// each session is charged separately only while it owns a lease below.
	handleQuota, err := reserveQuotaAccounts([]*QuotaAccount{s.processQuota, s.shareQuota}, QuotaUsage{StableHandles: 1})
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseSourceOpen, err
		return
	}
	var stable StableFile
	cleanup := true
	defer func() {
		if cleanup {
			revisionCleanup{source: stable, reservation: handleQuota}.run()
		}
	}()
	stable, err = s.source.OpenStable(ctx, record)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonOf(err), RevisionTraceCauseSourceOpen, err
		return
	}
	if err := stable.Verify(ctx); err != nil {
		result.comparison, result.cause = RevisionComparisonOf(err), RevisionTraceCauseVerification
		if result.comparison == RevisionComparisonMismatch {
			result.err = errors.Join(ErrRevisionStale, err)
		} else {
			result.err = fmt.Errorf("verify stable file before revision publication: %w", err)
		}
		return
	}
	expectedSize := record.Entry().ExpectedSize()
	if stable.ExactSize() != expectedSize {
		result.comparison, result.cause, result.err = RevisionComparisonMismatch, RevisionTraceCauseGeometry, ErrRevisionStale
		return
	}
	if stable.ModifiedTime() != record.Entry().ModifiedTime() {
		result.comparison, result.cause, result.err = RevisionComparisonMismatch, RevisionTraceCauseModifiedTime, ErrRevisionStale
		return
	}
	geometry, err := NewFileGeometry(expectedSize, s.chunkSize)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseGeometry, err
		return
	}
	descriptor, err := NewFileRevisionDescriptor(
		s.shareInstance, attempt.file, revisionID, geometry, record.Entry().ModifiedTime(),
	)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseGeometry, err
		return
	}
	result.revision = &revisionState{
		descriptor: descriptor, source: stable, handleQuota: handleQuota,
		leases: make(map[LeaseID]*leaseState), sessionHandles: make(map[*QuotaAccount]*sessionHandleState),
		graceUntil: s.clock.Now().Add(RevisionResumeGrace), lifecycle: revisionLifecycleActive,
	}
	result.comparison = RevisionComparisonMatch
	cleanup = false
}

// completeOpen holds s.mu while converting a provider result into tuple-global
// lifecycle state, then performs callbacks and physical cleanup after unlock.
func (s *RevisionStore) completeOpen(attempt *openAttempt, result openWorkResult) {
	var cleanups []revisionCleanup
	var admission *openAdmission
	invalidateCache := false
	metadataStopped := false
	s.mu.Lock()
	if attempt.completed {
		s.mu.Unlock()
		if result.revision != nil {
			revisionCleanup{source: result.revision.source, reservation: result.revision.handleQuota}.run()
		}
		return
	}
	attempt.completed = true
	current := s.opening[attempt.file] == attempt
	if result.comparison == RevisionComparisonMismatch && !result.identity.isZero() {
		var budgetErr error
		cleanups, budgetErr = s.invalidateIdentityLocked(result.identity, nil)
		invalidateCache = true
		if budgetErr != nil {
			metadataStopped = true
			result.err = errors.Join(result.err, budgetErr)
		}
	}
	if result.err == nil && result.revision != nil && current && !s.closed {
		switch {
		case s.revisionAdmissionStopped:
			result.err = fmt.Errorf("%w: revision admission stopped", ErrQuotaExceeded)
			metadataStopped = true
		case s.invalidated[result.identity] != nil:
			result.err = ErrRevisionDrift
			result.rejected = true
			result.cause = RevisionTraceCauseKnownInvalidation
		case s.revisions[attempt.file] != nil:
			result.err = errors.New("revision source raced an existing file revision")
		default:
			s.revisions[attempt.file] = result.revision
			attempt.revision = result.revision
		}
	}
	if result.err == nil && attempt.revision == nil {
		result.err = context.Canceled
	}
	attempt.err = result.err
	if result.err != nil || attempt.revision == nil {
		admission = attempt.ownerAdmission
		attempt.ownerAdmission = nil
	}
	if current && attempt.ownerAdmission == nil {
		delete(s.opening, attempt.file)
	}
	if result.revision != nil && attempt.revision != result.revision {
		result.revision.lifecycle = revisionLifecycleReleased
		result.revision.closed = true
		cleanups = append(cleanups, revisionCleanup{source: result.revision.source, reservation: result.revision.handleQuota})
		result.revision.source = nil
		result.revision.handleQuota = nil
	}
	s.mu.Unlock()
	for _, cleanup := range cleanups {
		cleanup.run()
	}
	if invalidateCache {
		s.invalidateRevisionCache(result.identity)
	}
	s.traceOpenCompletion(result, attempt)
	if metadataStopped {
		s.traceRevision(RevisionTraceStageMetadataBudgetStop, RevisionTraceCauseMetadataBudget, result.identity.file, result.identity.revision)
	}
	admission.release()
	// Closing done publishes both the result and completed rollback, so callers
	// never observe a failed open with admission still charged.
	close(attempt.done)
}

func (s *RevisionStore) traceOpenCompletion(result openWorkResult, attempt *openAttempt) {
	switch result.comparison {
	case RevisionComparisonMismatch:
		s.traceRevision(RevisionTraceStageMismatchInvalidation, result.cause, result.identity.file, result.identity.revision)
		return
	case RevisionComparisonUnavailable:
		s.traceRevision(RevisionTraceStageUnavailableRetry, result.cause, attempt.file, result.identity.revision)
		return
	case RevisionComparisonMatch, RevisionComparisonUnknown:
		// Rejection is decided during settlement, after the provider comparison,
		// so it is intentionally projected below the comparison-state switch.
	default:
		return
	}
	if result.rejected {
		s.traceRevision(RevisionTraceStageInvalidationRejection, result.cause, attempt.file, result.identity.revision)
		return
	}
	if attempt.revision != nil {
		s.traceRevision(RevisionTraceStageReopenMatch, RevisionTraceCauseUnknown, result.identity.file, result.identity.revision)
	}
}
