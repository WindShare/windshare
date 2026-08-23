package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type OpenRevisionRequest struct {
	FileID        catalog.FileID
	InitialRanges RangeSet
}

type OpenRevisionResult struct {
	FileID catalog.FileID
	Lease  RevisionLease
	Err    error
}

// LeaseEndKind records the evidence that ended one lease. Invalidation and
// store shutdown are revision-global authority transitions and deliberately do
// not use this contract.
type LeaseEndKind uint8

const (
	LeaseRelinquished LeaseEndKind = iota + 1
	LeaseUndelivered
	LeaseDetached
)

func (kind LeaseEndKind) valid() bool {
	return kind >= LeaseRelinquished && kind <= LeaseDetached
}

func (s *RevisionStore) OpenRevisions(ctx context.Context, requests []OpenRevisionRequest, session *revisioncapacity.SessionRegistration) ([]OpenRevisionResult, error) {
	if len(requests) > MaxOpenRevisionBatch {
		return nil, ErrOpenBatchLimit
	}
	totalRanges := 0
	for _, request := range requests {
		if request.InitialRanges.Len() > MaxInitialRangesPerFile || totalRanges > MaxInitialRangesPerRequest-request.InitialRanges.Len() {
			return nil, ErrInitialRangeLimit
		}
		totalRanges += request.InitialRanges.Len()
	}
	results := make([]OpenRevisionResult, len(requests))
	for index, request := range requests {
		results[index].FileID = request.FileID
		lease, err := s.OpenRevision(ctx, request.FileID, session)
		if err == nil && !request.InitialRanges.IsEmpty() {
			_, err = lease.Descriptor().Geometry().BlocksForRanges(request.InitialRanges)
			if err != nil {
				_ = s.EndLease(lease.ID(), LeaseUndelivered)
				lease = RevisionLease{}
			}
		}
		results[index].Lease, results[index].Err = lease, err
	}
	return results, nil
}

func (s *RevisionStore) RenewLease(id LeaseID) (RevisionLease, error) {
	now := s.clock.Now()
	s.reap(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RevisionLease{}, ErrRevisionStoreClosed
	}
	state := s.leases[id]
	if state == nil {
		return RevisionLease{}, s.leaseTombstoneErrorLocked(id)
	}
	if state.status == leaseDrifted || state.revision.lifecycle == revisionLifecycleInvalidated {
		return RevisionLease{}, ErrRevisionDrift
	}
	if state.status != leaseActive || !now.Before(state.expiresAt) {
		return RevisionLease{}, ErrLeaseExpired
	}
	if state.expiresAt.Sub(now) > LeaseRenewWindow {
		return RevisionLease{}, ErrRenewTooEarly
	}
	maximum := state.createdAt.Add(MaxLeaseLifetime)
	if !now.Before(maximum) {
		return RevisionLease{}, ErrLeaseLifetime
	}
	next := now.Add(LeaseTTL)
	if next.After(maximum) {
		// Wire timing is frozen at TTL=120s/RenewAfter=60s. Refusing a
		// truncated final renewal keeps local lease authority and authenticated
		// lease results on the same contract.
		return RevisionLease{}, ErrLeaseLifetime
	}
	state.expiresAt = next
	state.lease.ttl = LeaseTTL
	state.lease.renewAfter = LeaseTTL - LeaseRenewWindow
	return state.lease, nil
}

func (s *RevisionStore) EndLease(id LeaseID, kind LeaseEndKind) error {
	if !kind.valid() {
		return ErrInvalidLeaseEnd
	}
	now := s.clock.Now()
	// Expiry is authoritative detachment. Reaping first prevents a later caller
	// from replacing that evidence or extending its recovery deadline.
	s.reap(now)
	s.mu.Lock()
	state := s.leases[id]
	if state == nil {
		s.mu.Unlock()
		return nil
	}
	releases := capacityChargeRelease{}
	sessionID := state.session.SessionID()
	if state.status == leaseActive {
		releases = s.endLeaseLocked(state, kind, now)
	}
	delete(s.leases, id)
	delete(state.revision.leases, id)
	s.rememberLeaseTombstoneLocked(id, leaseEnded)
	transition, retired := s.retireRevisionIfIdleLocked(state.revision, now)
	identity := state.revision.identity()
	s.mu.Unlock()
	err := errors.Join(releases.run(), s.runRevisionTransition(transition))
	s.traceLeaseRevision(RevisionTraceStageLeaseSettlement, leaseEndTraceCause(kind), identity.file, identity.revision, id, sessionID)
	if retired {
		s.traceLeaseRevision(RevisionTraceStageCleanRelease, leaseEndTraceCause(kind), identity.file, identity.revision, id, sessionID)
	}
	return err
}

// ValidateLease is the authorization boundary for cache hits. A sealed object
// may outlive the lease that populated the cache, so serving it without
// consulting RevisionStore would silently bypass expiry and drift revocation.
func (s *RevisionStore) ValidateLease(id LeaseID, descriptor FileRevisionDescriptor) error {
	if id.IsZero() || descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() {
		return ErrInvalidLease
	}
	now := s.clock.Now()
	s.reap(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrRevisionStoreClosed
	}
	state := s.leases[id]
	if state == nil {
		return s.leaseTombstoneErrorLocked(id)
	}
	if state.status == leaseDrifted || state.revision.lifecycle == revisionLifecycleInvalidated {
		return ErrRevisionDrift
	}
	if state.status != leaseActive || !now.Before(state.expiresAt) {
		return ErrLeaseExpired
	}
	if state.lease.Descriptor() != descriptor {
		return ErrInvalidLease
	}
	return nil
}

func (s *RevisionStore) leaseTombstoneErrorLocked(id LeaseID) error {
	switch s.leaseTombstones[id] {
	case leaseEnded:
		return ErrLeaseExpired
	case leaseDrifted:
		return ErrRevisionDrift
	default:
		return ErrInvalidLease
	}
}

func (s *RevisionStore) endLeaseLocked(state *leaseState, kind LeaseEndKind, endedAt time.Time) capacityChargeRelease {
	if state.status != leaseActive {
		return capacityChargeRelease{}
	}
	state.status = leaseEnded
	state.revision.activeLeases--
	state.endedAt = endedAt
	if kind == LeaseDetached {
		recoveryUntil := endedAt.Add(RevisionResumeGrace)
		if recoveryUntil.After(state.revision.recoveryUntil) {
			state.revision.recoveryUntil = recoveryUntil
		}
	}
	release := capacityChargeRelease{active: state.activeCharge}
	state.activeCharge = revisioncapacity.ActiveLeaseCharge{}
	release.sessionHandle = s.releaseSessionHandleLocked(state)
	return release
}

type revisionTransitionKind uint8

const (
	revisionTransitionNone revisionTransitionKind = iota
	revisionTransitionPublishIdle
	revisionTransitionClose
)

type revisionTransition struct {
	kind      revisionTransitionKind
	revision  *revisionState
	token     revisioncapacity.CandidateToken
	candidate revisioncapacity.IdleCandidate
	cleanup   revisionCleanup
}

func (s *RevisionStore) retireRevisionIfIdleLocked(revision *revisionState, now time.Time) (revisionTransition, bool) {
	if revision == nil || revision.lifecycle != revisionLifecycleActive || s.hasActiveLeaseLocked(revision) || revision.admissionDone != nil {
		return revisionTransition{}, false
	}
	if attempt := s.opening[revision.descriptor.FileID()]; attempt != nil && attempt.revision == revision {
		return revisionTransition{}, false
	}
	if !s.closed && !revision.recoveryUntil.IsZero() && now.Before(revision.recoveryUntil) {
		if revision.readers != 0 {
			return revisionTransition{}, false
		}
		if revision.idleToken != "" {
			return revisionTransition{}, false
		}
		revision.lifecycleGeneration++
		token := revisioncapacity.CandidateToken(fmt.Sprintf(
			"%x:%x:%d", revision.descriptor.FileID().Bytes(), revision.descriptor.FileRevision().Bytes(), revision.lifecycleGeneration,
		))
		revision.idleToken = token
		s.idleRevisions[token] = revision
		s.capacityWG.Add(1)
		return revisionTransition{
			kind: revisionTransitionPublishIdle, revision: revision, token: token,
			candidate: revisioncapacity.IdleCandidate{
				Token: token, RevisionID: capacityRevisionID(revision.identity()),
				RecoveryUntil: revision.recoveryUntil, LifecycleGeneration: revision.lifecycleGeneration,
				StableHandle: revision.handleCharge,
			},
		}, false
	}
	if s.revisions[revision.descriptor.FileID()] == revision {
		delete(s.revisions, revision.descriptor.FileID())
	}
	revision.lifecycleGeneration++
	revision.lifecycle = revisionLifecycleReleased
	revision.closePending = true
	transition := revisionTransition{kind: revisionTransitionClose, revision: revision, token: revision.idleToken}
	if transition.token != "" {
		delete(s.idleRevisions, transition.token)
	}
	revision.idleToken = ""
	if revision.readers != 0 {
		return revisionTransition{}, true
	}
	if transition.token == "" && revision.source != nil && !revision.closing && !revision.closed {
		revision.closing = true
		transition.cleanup = revisionCleanup{store: s, revision: revision, source: revision.source, stableCharge: revision.handleCharge}
		revision.source = nil
		revision.handleCharge = revisioncapacity.StableHandleCharge{}
	}
	return transition, true
}

func (s *RevisionStore) runRevisionTransition(transition revisionTransition) error {
	switch transition.kind {
	case revisionTransitionNone:
		return nil
	case revisionTransitionPublishIdle:
		defer s.capacityWG.Done()
		publishErr := s.capacity.PublishIdle(transition.candidate)
		withdraw := false
		var fallback revisionTransition
		s.mu.Lock()
		valid := !s.closed && transition.revision.lifecycle == revisionLifecycleActive &&
			transition.revision.idleToken == transition.token &&
			transition.revision.lifecycleGeneration == transition.candidate.LifecycleGeneration &&
			transition.revision.readers == 0 && !s.hasActiveLeaseLocked(transition.revision) &&
			transition.revision.admissionDone == nil && s.clock.Now().Before(transition.revision.recoveryUntil)
		if publishErr != nil && valid {
			delete(s.idleRevisions, transition.token)
			transition.revision.idleToken = ""
			transition.revision.recoveryUntil = time.Time{}
			fallback, _ = s.retireRevisionIfIdleLocked(transition.revision, s.clock.Now())
		} else if publishErr == nil && !valid {
			withdraw = true
			if transition.revision.idleToken == transition.token &&
				transition.revision.lifecycleGeneration == transition.candidate.LifecycleGeneration &&
				!s.clock.Now().Before(transition.revision.recoveryUntil) {
				delete(s.idleRevisions, transition.token)
				transition.revision.idleToken = ""
				transition.revision.recoveryUntil = time.Time{}
				fallback, _ = s.retireRevisionIfIdleLocked(transition.revision, s.clock.Now())
			}
		}
		s.mu.Unlock()
		if withdraw {
			s.capacity.WithdrawIdle(transition.token)
		}
		return errors.Join(publishErr, s.runRevisionTransition(fallback))
	case revisionTransitionClose:
		if transition.token != "" {
			transition.cleanup = revisionCleanup{store: s, revision: transition.revision, idleToken: transition.token}
		}
		err := transition.cleanup.run()
		return err
	default:
		return errors.New("revision transition kind is invalid")
	}
}

func leaseEndTraceCause(kind LeaseEndKind) RevisionTraceCause {
	switch kind {
	case LeaseRelinquished:
		return RevisionTraceCauseRelinquished
	case LeaseUndelivered:
		return RevisionTraceCauseUndelivered
	case LeaseDetached:
		return RevisionTraceCauseDetached
	default:
		return RevisionTraceCauseUnknown
	}
}

func (s *RevisionStore) releaseSessionHandleLocked(state *leaseState) revisioncapacity.SessionHandleCharge {
	if state.session == nil {
		return revisioncapacity.SessionHandleCharge{}
	}
	handle := state.revision.sessionHandles[state.session]
	if handle == nil || handle.leases == 0 {
		state.session = nil
		return revisioncapacity.SessionHandleCharge{}
	}
	handle.leases--
	var charge revisioncapacity.SessionHandleCharge
	if handle.leases == 0 {
		charge = handle.charge
		delete(state.revision.sessionHandles, state.session)
	}
	state.session = nil
	return charge
}

func releaseAllSessionHandlesLocked(revision *revisionState) []capacityChargeRelease {
	releases := make([]capacityChargeRelease, 0, len(revision.sessionHandles))
	for session, handle := range revision.sessionHandles {
		releases = append(releases, capacityChargeRelease{sessionHandle: handle.charge})
		delete(revision.sessionHandles, session)
	}
	return releases
}

func (s *RevisionStore) hasActiveLeaseLocked(revision *revisionState) bool {
	return revision != nil && revision.activeLeases != 0
}

func (s *RevisionStore) ReadBlock(ctx context.Context, leaseID LeaseID, ref BlockRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	s.reap(now)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrRevisionStoreClosed
	}
	lease := s.leases[leaseID]
	if lease == nil {
		err := s.leaseTombstoneErrorLocked(leaseID)
		s.mu.Unlock()
		return nil, err
	}
	if lease.status == leaseDrifted || lease.revision.lifecycle == revisionLifecycleInvalidated {
		s.mu.Unlock()
		return nil, ErrRevisionDrift
	}
	if lease.status != leaseActive || !now.Before(lease.expiresAt) {
		s.mu.Unlock()
		return nil, ErrLeaseExpired
	}
	descriptor := lease.revision.descriptor
	if ref.FileID() != descriptor.FileID() || ref.FileRevision() != descriptor.FileRevision() || ref.LocalBlockIndex() >= descriptor.Geometry().BlockCount() {
		s.mu.Unlock()
		return nil, ErrInvalidBlockRef
	}
	offset, _ := descriptor.Geometry().BlockOffset(ref.LocalBlockIndex())
	plainLength, _ := descriptor.Geometry().BlockPlainLength(ref.LocalBlockIndex())
	revision := lease.revision
	revision.readers++
	s.readingRevisions[revision] = struct{}{}
	s.readWG.Add(1)
	s.mu.Unlock()
	defer s.readWG.Done()

	destination := make([]byte, plainLength)
	comparison, readErr := readStableBlock(ctx, revision.source, destination, offset)
	drifted, cleanups, transitions, releases, invalidate, budgetErr := s.finishRead(revision, comparison)
	for _, release := range releases {
		_ = release.run()
	}
	for _, cleanup := range cleanups {
		_ = cleanup.run()
	}
	for _, transition := range transitions {
		_ = s.runRevisionTransition(transition)
	}
	if invalidate {
		s.invalidateRevisionCache(revisionIdentity{file: descriptor.FileID(), revision: descriptor.FileRevision()})
	}
	if comparison == RevisionComparisonMismatch {
		s.traceRevision(RevisionTraceStageMismatchInvalidation, RevisionTraceCauseActiveRead, descriptor.FileID(), descriptor.FileRevision())
	}
	if budgetErr != nil {
		s.traceRevision(RevisionTraceStageMetadataBudgetStop, RevisionTraceCauseMetadataBudget, descriptor.FileID(), descriptor.FileRevision())
	}
	if drifted {
		return nil, ErrRevisionDrift
	}
	if readErr != nil {
		return nil, readErr
	}
	return destination, nil
}

func readStableBlock(ctx context.Context, source StableFile, destination []byte, offset uint64) (comparison RevisionComparison, readErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			readErr = fmt.Errorf("stable file read panicked: %v\n%s", recovered, debug.Stack())
			comparison = RevisionComparisonUnavailable
		}
	}()
	if readErr = source.Verify(ctx); readErr != nil {
		return RevisionComparisonOf(readErr), readErr
	}
	count, readErr := source.ReadAt(ctx, destination, offset)
	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		return RevisionComparisonUnavailable, readErr
	}
	shortEOF := count != len(destination) && errors.Is(readErr, io.EOF)
	if count == len(destination) && errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if count != len(destination) && readErr == nil {
		readErr = io.ErrUnexpectedEOF
	}
	verifyErr := source.Verify(ctx)
	if RevisionComparisonOf(verifyErr) == RevisionComparisonMismatch {
		return RevisionComparisonMismatch, verifyErr
	}
	if shortEOF || RevisionComparisonOf(readErr) == RevisionComparisonMismatch {
		return RevisionComparisonMismatch, readErr
	}
	if readErr != nil {
		return RevisionComparisonUnavailable, readErr
	}
	if verifyErr != nil {
		return RevisionComparisonUnavailable, verifyErr
	}
	return RevisionComparisonMatch, nil
}

func (s *RevisionStore) finishRead(revision *revisionState, comparison RevisionComparison) (
	bool,
	[]revisionCleanup,
	[]revisionTransition,
	[]capacityChargeRelease,
	bool,
	error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision.readers--
	if revision.readers == 0 {
		delete(s.readingRevisions, revision)
	}
	var cleanups []revisionCleanup
	var transitions []revisionTransition
	var releases []capacityChargeRelease
	var budgetErr error
	invalidate := comparison == RevisionComparisonMismatch
	if invalidate {
		cleanups, releases, budgetErr = s.invalidateIdentityLocked(revision.identity(), revision)
	}
	if !invalidate {
		if transition, _ := s.retireRevisionIfIdleLocked(revision, s.clock.Now()); transition.kind != revisionTransitionNone {
			transitions = append(transitions, transition)
		}
	}
	_, recorded := s.invalidated[revision.identity()]
	drifted := revision.lifecycle == revisionLifecycleInvalidated || recorded
	if revision.closePending && revision.readers == 0 && !revision.closed {
		if revision.source != nil && !revision.closing {
			revision.closing = true
			cleanups = append(cleanups, revisionCleanup{store: s, revision: revision, source: revision.source, stableCharge: revision.handleCharge})
			revision.source = nil
			revision.handleCharge = revisioncapacity.StableHandleCharge{}
		}
	}
	return drifted, cleanups, transitions, releases, invalidate, budgetErr
}

func (s *RevisionStore) reap(now time.Time) {
	cleanups := make([]revisionCleanup, 0)
	transitions := make([]revisionTransition, 0)
	releases := make([]capacityChargeRelease, 0)
	released := make([]revisionIdentity, 0)
	type detachedLeaseTrace struct {
		identity revisionIdentity
		leaseID  LeaseID
		session  revisioncapacity.SessionID
	}
	detached := make([]detachedLeaseTrace, 0)
	s.mu.Lock()
	expired := make([]LeaseID, 0)
	for id, lease := range s.leases {
		if lease.status == leaseActive && !now.Before(lease.expiresAt) {
			sessionID := lease.session.SessionID()
			releases = append(releases, s.endLeaseLocked(lease, LeaseDetached, lease.expiresAt))
			expired = append(expired, id)
			detached = append(detached, detachedLeaseTrace{
				identity: lease.revision.identity(), leaseID: id, session: sessionID,
			})
		}
	}
	for _, id := range expired {
		lease := s.leases[id]
		delete(s.leases, id)
		delete(lease.revision.leases, id)
		s.rememberLeaseTombstoneLocked(id, leaseEnded)
	}
	for file, revision := range s.revisions {
		transition, retired := s.retireRevisionIfIdleLocked(revision, now)
		if retired {
			delete(s.revisions, file)
			released = append(released, revision.identity())
			if transition.kind != revisionTransitionNone {
				transitions = append(transitions, transition)
			}
		} else if transition.kind != revisionTransitionNone {
			transitions = append(transitions, transition)
		}
	}
	s.mu.Unlock()
	for _, release := range releases {
		_ = release.run()
	}
	for _, cleanup := range cleanups {
		_ = cleanup.run()
	}
	for _, transition := range transitions {
		_ = s.runRevisionTransition(transition)
	}
	for _, trace := range detached {
		s.traceLeaseRevision(
			RevisionTraceStageLeaseSettlement, RevisionTraceCauseDetached,
			trace.identity.file, trace.identity.revision, trace.leaseID, trace.session,
		)
	}
	for _, identity := range released {
		s.traceRevision(RevisionTraceStageCleanRelease, RevisionTraceCauseDetached, identity.file, identity.revision)
	}
}
