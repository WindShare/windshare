package content

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

var errRetryOpenForWaitingSession = errors.New("retry coalesced open for waiting session")

type openAttempt struct {
	file           catalog.FileID
	done           chan struct{}
	cancel         context.CancelFunc
	waiters        int
	completed      bool
	ownerAbandoned bool
	ownerSession   *revisioncapacity.SessionRegistration
	ownerLease     RevisionLease
	revision       *revisionState
	err            error
}

type openWorkResult struct {
	stable     StableFile
	descriptor FileRevisionDescriptor
	identity   revisionIdentity
	leaseID    LeaseID
	grant      revisioncapacity.AdmissionGrant
	comparison RevisionComparison
	cause      RevisionTraceCause
	rejected   bool
	err        error
}

func (s *RevisionStore) OpenRevision(
	ctx context.Context,
	file catalog.FileID,
	session *revisioncapacity.SessionRegistration,
) (RevisionLease, error) {
	if err := ctx.Err(); err != nil {
		return RevisionLease{}, err
	}
	if file.IsZero() || session == nil {
		return RevisionLease{}, errors.New("open revision requires a file identity and capacity session registration")
	}
	s.reap(s.clock.Now())

	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return RevisionLease{}, ErrRevisionStoreClosed
		}
		if revision := s.revisions[file]; revision != nil && revision.lifecycle == revisionLifecycleActive && !revision.closed && !revision.closing {
			if revision.admissionDone != nil {
				done := revision.admissionDone
				s.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return RevisionLease{}, ctx.Err()
				}
			}
			s.mu.Unlock()
			lease, err := s.admitResidentRevision(ctx, revision, session)
			if err == nil {
				descriptor := lease.Descriptor()
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
			lease, err := s.awaitOpen(ctx, attempt, false)
			if errors.Is(err, errRetryOpenForWaitingSession) {
				continue
			}
			return lease, err
		}
		attemptContext, cancel := context.WithCancel(s.capacityContext)
		attempt := &openAttempt{
			file: file, done: make(chan struct{}), cancel: cancel, waiters: 1, ownerSession: session,
		}
		s.opening[file] = attempt
		s.openWG.Add(1)
		s.mu.Unlock()
		go s.runOpen(attemptContext, attempt)
		return s.awaitOpen(ctx, attempt, true)
	}
}

func (s *RevisionStore) awaitOpen(
	ctx context.Context,
	attempt *openAttempt,
	owner bool,
) (RevisionLease, error) {
	select {
	case <-attempt.done:
		s.mu.Lock()
		attempt.waiters--
		err := attempt.err
		lease := attempt.ownerLease
		s.mu.Unlock()
		if err != nil {
			if !owner && errors.Is(err, revisioncapacity.ErrRegistrationClosing) {
				// Coalescing is store-scoped, so one departed session must not make
				// another session inherit an otherwise healthy open attempt's failure.
				return RevisionLease{}, errRetryOpenForWaitingSession
			}
			return RevisionLease{}, err
		}
		if owner && !lease.ID().IsZero() {
			return lease, nil
		}
		// A waiter never inherits another session's charge. The outer loop
		// re-enters the resident transition under its authenticated session.
		return RevisionLease{}, errRetryOpenForWaitingSession
	case <-ctx.Done():
		var abandonedLease LeaseID
		s.mu.Lock()
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
		s.mu.Unlock()
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

	s.mu.Lock()
	_, invalidated := s.invalidated[result.identity]
	admissionStopped := s.revisionAdmissionStopped
	current := s.opening[attempt.file] == attempt && !s.closed
	var err error
	if current && !invalidated && !admissionStopped {
		result.leaseID, err = s.newLeaseIDLocked()
	}
	s.mu.Unlock()
	if !current {
		result.err = context.Canceled
		return
	}
	if invalidated {
		result.rejected, result.cause, result.err = true, RevisionTraceCauseKnownInvalidation, ErrRevisionDrift
		return
	}
	if admissionStopped {
		result.rejected, result.cause = true, RevisionTraceCauseMetadataBudget
		result.err = fmt.Errorf("%w: revision admission stopped", ErrQuotaExceeded)
		return
	}
	if err != nil {
		result.err = err
		return
	}

	result.grant, err = s.capacity.Admit(ctx, revisioncapacity.AdmissionRequest{
		Kind:       revisioncapacity.AdmissionNewRevision,
		RevisionID: capacityRevisionID(result.identity),
		Session:    attempt.ownerSession,
	})
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCapacity, err
		return
	}
	cleanupGrant := true
	defer func() {
		if cleanupGrant {
			result.err = errors.Join(result.err, cleanupOpenWork(result.stable, result.grant))
			result.stable = nil
			result.grant = revisioncapacity.AdmissionGrant{}
		}
	}()
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
	result.comparison = RevisionComparisonMatch
	cleanupGrant = false
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
	revisionID, err := s.revisionDeriver.DeriveRevision(evidence)
	if err != nil {
		result.comparison, result.cause, result.err = RevisionComparisonUnavailable, RevisionTraceCauseCatalog, err
		return catalog.NodeRecord{}, false
	}
	result.identity = revisionIdentity{file: attempt.file, revision: revisionID}
	return record, true
}

func cleanupOpenWork(stable StableFile, grant revisioncapacity.AdmissionGrant) (err error) {
	terminal := stable == nil
	if stable != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("stable file close panicked: %v\n%s", recovered, debug.Stack())
				}
			}()
			err = stable.Close()
			terminal = true
		}()
	}
	if grant.DecisionID() == "" {
		return err
	}
	if terminal {
		return errors.Join(err, grant.Abort())
	}
	return errors.Join(err, grant.QuarantineStableHandle(err))
}

// completeOpen confirms store lifecycle, commits coordinator ownership without
// s.mu, and then publishes the source and first lease in one local transition.
func (s *RevisionStore) completeOpen(attempt *openAttempt, result openWorkResult) {
	var cleanups []revisionCleanup
	var releases []capacityChargeRelease
	invalidateCache := false
	metadataStopped := false
	s.mu.Lock()
	if attempt.completed {
		s.mu.Unlock()
		_ = cleanupOpenWork(result.stable, result.grant)
		return
	}
	attempt.completed = true
	current := s.opening[attempt.file] == attempt
	if result.comparison == RevisionComparisonMismatch && !result.identity.isZero() {
		var budgetErr error
		cleanups, releases, budgetErr = s.invalidateIdentityLocked(result.identity, nil)
		invalidateCache = true
		if budgetErr != nil {
			metadataStopped = true
			result.err = errors.Join(result.err, budgetErr)
		}
	}
	if result.err == nil && result.stable != nil && current && !s.closed {
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
		}
	}
	if result.err == nil && (!current || s.closed || result.stable == nil) {
		result.err = context.Canceled
	}
	s.mu.Unlock()

	for _, cleanup := range cleanups {
		result.err = errors.Join(result.err, cleanup.run())
	}
	for _, release := range releases {
		result.err = errors.Join(result.err, release.run())
	}
	if invalidateCache {
		s.invalidateRevisionCache(result.identity)
	}
	if result.err == nil {
		result.err = s.commitOpenResult(attempt, &result)
	}
	if result.err != nil && result.stable != nil {
		result.err = errors.Join(result.err, cleanupOpenWork(result.stable, result.grant))
		result.stable = nil
		result.grant = revisioncapacity.AdmissionGrant{}
	}

	s.mu.Lock()
	attempt.err = result.err
	if s.opening[attempt.file] == attempt {
		delete(s.opening, attempt.file)
	}
	abandonedLease := LeaseID{}
	if attempt.ownerAbandoned {
		abandonedLease = attempt.ownerLease.ID()
	}
	s.mu.Unlock()
	if !abandonedLease.IsZero() {
		_ = s.EndLease(abandonedLease, LeaseUndelivered)
	}
	s.traceOpenCompletion(result, attempt)
	if metadataStopped {
		s.traceRevision(RevisionTraceStageMetadataBudgetStop, RevisionTraceCauseMetadataBudget, result.identity.file, result.identity.revision)
	}
	close(attempt.done)
}

func (s *RevisionStore) commitOpenResult(attempt *openAttempt, result *openWorkResult) error {
	charges, err := result.grant.Commit()
	if err != nil {
		return err
	}
	stableCharge, stableOK := charges.StableHandle()
	activeCharge, activeOK := charges.ActiveLease()
	sessionCharge, sessionOK := charges.SessionHandle()
	if !stableOK || !activeOK || !sessionOK {
		return errors.Join(errors.New("new revision admission returned incomplete charges"), cleanupCommittedOpen(result.stable, charges))
	}

	s.mu.Lock()
	if s.closed || s.opening[attempt.file] != attempt || s.invalidated[result.identity] != nil || s.revisions[attempt.file] != nil {
		s.mu.Unlock()
		return errors.Join(context.Canceled, cleanupCommittedOpen(result.stable, charges))
	}
	revision := &revisionState{
		descriptor:          result.descriptor,
		source:              result.stable,
		handleCharge:        stableCharge,
		leases:              make(map[LeaseID]*leaseState),
		sessionHandles:      make(map[*revisioncapacity.SessionRegistration]*sessionHandleState),
		lifecycleGeneration: 1,
		lifecycle:           revisionLifecycleActive,
	}
	lease, publishErr := s.publishLeaseLocked(
		revision, attempt.ownerSession, s.clock.Now(), result.leaseID, activeCharge, sessionCharge,
	)
	if publishErr == nil {
		s.revisions[attempt.file] = revision
		attempt.revision = revision
		attempt.ownerLease = lease
		result.stable = nil
		result.grant = revisioncapacity.AdmissionGrant{}
	}
	s.mu.Unlock()
	if publishErr != nil {
		return errors.Join(publishErr, cleanupCommittedOpen(result.stable, charges))
	}
	return nil
}

func cleanupCommittedOpen(stable StableFile, charges revisioncapacity.AdmissionCharges) error {
	active, _ := charges.ActiveLease()
	session, _ := charges.SessionHandle()
	stableCharge, _ := charges.StableHandle()
	err := capacityChargeRelease{active: active, sessionHandle: session}.run()
	return errors.Join(err, revisionCleanup{source: stable, stableCharge: stableCharge}.run())
}

func (s *RevisionStore) admitResidentRevision(
	ctx context.Context,
	revision *revisionState,
	session *revisioncapacity.SessionRegistration,
) (RevisionLease, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return RevisionLease{}, ErrRevisionStoreClosed
	}
	if revision.lifecycle != revisionLifecycleActive || revision.closed || revision.closing ||
		s.revisions[revision.descriptor.FileID()] != revision || revision.admissionDone != nil {
		s.mu.Unlock()
		return RevisionLease{}, ErrRevisionDrift
	}
	leaseID, err := s.newLeaseIDLocked()
	if err != nil {
		s.mu.Unlock()
		return RevisionLease{}, err
	}
	kind := revisioncapacity.AdmissionAdditionalSessionLease
	if revision.sessionHandles[session] == nil {
		kind = revisioncapacity.AdmissionFirstSessionLease
	}
	idleToken := revision.idleToken
	if idleToken != "" {
		delete(s.idleRevisions, idleToken)
	}
	revision.idleToken = ""
	revision.lifecycleGeneration++
	generation := revision.lifecycleGeneration
	done := make(chan struct{})
	revision.admissionDone = done
	s.capacityWG.Add(1)
	s.mu.Unlock()
	defer s.capacityWG.Done()
	if idleToken != "" {
		s.capacity.WithdrawIdle(idleToken)
		// Withdraw marks an in-flight claim stale but does not resolve its charge.
		// Join that ownership before resident admission can fail and republish the
		// same physical handle under a new lifecycle generation.
		if err := s.capacity.WaitForReclaims(); err != nil {
			return RevisionLease{}, errors.Join(err, s.finishResidentAdmission(revision, done, generation))
		}
	}

	admissionContext, cancel := s.capacityAdmissionContext(ctx)
	grant, admitErr := s.capacity.Admit(admissionContext, revisioncapacity.AdmissionRequest{
		Kind: kind, RevisionID: capacityRevisionID(revision.identity()), Session: session,
	})
	cancel()
	if admitErr != nil {
		return RevisionLease{}, errors.Join(admitErr, s.finishResidentAdmission(revision, done, generation))
	}

	s.mu.Lock()
	valid := s.residentAdmissionValidLocked(revision, done, generation)
	s.mu.Unlock()
	if !valid {
		return RevisionLease{}, errors.Join(
			ErrRevisionDrift,
			grant.Abort(),
			s.finishResidentAdmission(revision, done, generation),
		)
	}
	charges, commitErr := grant.Commit()
	if commitErr != nil {
		return RevisionLease{}, errors.Join(commitErr, s.finishResidentAdmission(revision, done, generation))
	}
	activeCharge, activeOK := charges.ActiveLease()
	sessionCharge, sessionOK := charges.SessionHandle()
	if !activeOK || kind == revisioncapacity.AdmissionFirstSessionLease && !sessionOK ||
		kind == revisioncapacity.AdmissionAdditionalSessionLease && sessionOK {
		return RevisionLease{}, errors.Join(
			errors.New("resident revision admission returned inconsistent charges"),
			charges.Release(),
			s.finishResidentAdmission(revision, done, generation),
		)
	}

	s.mu.Lock()
	if !s.residentAdmissionValidLocked(revision, done, generation) {
		s.mu.Unlock()
		return RevisionLease{}, errors.Join(
			ErrRevisionDrift,
			charges.Release(),
			s.finishResidentAdmission(revision, done, generation),
		)
	}
	lease, publishErr := s.publishLeaseLocked(revision, session, s.clock.Now(), leaseID, activeCharge, sessionCharge)
	revision.admissionDone = nil
	close(done)
	var transition revisionTransition
	if publishErr != nil {
		transition, _ = s.retireRevisionIfIdleLocked(revision, s.clock.Now())
	}
	s.mu.Unlock()
	if publishErr != nil {
		return RevisionLease{}, errors.Join(publishErr, charges.Release(), s.runRevisionTransition(transition))
	}
	return lease, nil
}

func (s *RevisionStore) residentAdmissionValidLocked(revision *revisionState, done chan struct{}, generation uint64) bool {
	return !s.closed && revision.lifecycle == revisionLifecycleActive && !revision.closed && !revision.closing &&
		revision.admissionDone == done && revision.lifecycleGeneration == generation &&
		s.revisions[revision.descriptor.FileID()] == revision
}

func (s *RevisionStore) finishResidentAdmission(revision *revisionState, done chan struct{}, generation uint64) error {
	var transition revisionTransition
	s.mu.Lock()
	if revision.admissionDone == done {
		revision.admissionDone = nil
		close(done)
		if revision.lifecycleGeneration == generation {
			transition, _ = s.retireRevisionIfIdleLocked(revision, s.clock.Now())
		}
	}
	s.mu.Unlock()
	return s.runRevisionTransition(transition)
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
