package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/windshare/windshare/core/catalog"
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

func (s *RevisionStore) OpenRevisions(ctx context.Context, requests []OpenRevisionRequest, sessionQuota *QuotaAccount) ([]OpenRevisionResult, error) {
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
		lease, err := s.OpenRevision(ctx, request.FileID, sessionQuota)
		if err == nil && !request.InitialRanges.IsEmpty() {
			_, err = lease.Descriptor().Geometry().BlocksForRanges(request.InitialRanges)
			if err != nil {
				_ = s.ReleaseLease(lease.ID())
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

func (s *RevisionStore) ReleaseLease(id LeaseID) error {
	now := s.clock.Now()
	// Reaping first anchors grace to the authoritative expiry even when a peer
	// sends RELEASE long after its lease ceased to be valid.
	s.reap(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.leases[id]
	if state == nil {
		return nil
	}
	if state.status == leaseActive {
		s.releaseLeaseLocked(state, now, leaseExpired)
	}
	delete(s.leases, id)
	delete(state.revision.leases, id)
	s.rememberLeaseTombstoneLocked(id, leaseExpired)
	return nil
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
	case leaseExpired:
		return ErrLeaseExpired
	case leaseDrifted:
		return ErrRevisionDrift
	default:
		return ErrInvalidLease
	}
}

func (s *RevisionStore) releaseLeaseLocked(state *leaseState, endedAt time.Time, status leaseStatus) {
	if state.status != leaseActive {
		return
	}
	state.status = status
	state.endedAt = endedAt
	state.quota.Release()
	state.quota = nil
	s.releaseSessionHandleLocked(state)
	if !s.hasActiveLeaseLocked(state.revision) {
		latestEnd := endedAt
		for _, lease := range state.revision.leases {
			if lease.status != leaseActive && lease.endedAt.After(latestEnd) {
				latestEnd = lease.endedAt
			}
		}
		state.revision.graceUntil = latestEnd.Add(RevisionResumeGrace)
	}
}

func (s *RevisionStore) releaseSessionHandleLocked(state *leaseState) {
	if state.sessionQuota == nil {
		return
	}
	handle := state.revision.sessionHandles[state.sessionQuota]
	if handle == nil || handle.leases == 0 {
		state.sessionQuota = nil
		return
	}
	handle.leases--
	if handle.leases == 0 {
		handle.quota.Release()
		delete(state.revision.sessionHandles, state.sessionQuota)
	}
	state.sessionQuota = nil
}

func releaseAllSessionHandlesLocked(revision *revisionState) {
	for session, handle := range revision.sessionHandles {
		handle.quota.Release()
		delete(revision.sessionHandles, session)
	}
}

func (s *RevisionStore) hasActiveLeaseLocked(revision *revisionState) bool {
	for _, lease := range revision.leases {
		if lease.status == leaseActive {
			return true
		}
	}
	return false
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
	drifted, cleanups, invalidate, budgetErr := s.finishRead(revision, comparison)
	for _, cleanup := range cleanups {
		cleanup.run()
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

func (s *RevisionStore) finishRead(revision *revisionState, comparison RevisionComparison) (bool, []revisionCleanup, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision.readers--
	if revision.readers == 0 {
		delete(s.readingRevisions, revision)
	}
	var cleanups []revisionCleanup
	var budgetErr error
	invalidate := comparison == RevisionComparisonMismatch
	if invalidate {
		cleanups, budgetErr = s.invalidateIdentityLocked(revision.identity(), revision)
	}
	_, recorded := s.invalidated[revision.identity()]
	drifted := revision.lifecycle == revisionLifecycleInvalidated || recorded
	if revision.closePending && revision.readers == 0 && !revision.closed {
		revision.closed = true
		cleanups = append(cleanups, revisionCleanup{source: revision.source, reservation: revision.handleQuota})
		revision.source = nil
		revision.handleQuota = nil
	}
	return drifted, cleanups, invalidate, budgetErr
}

func (s *RevisionStore) reap(now time.Time) {
	cleanups := make([]revisionCleanup, 0)
	released := make([]revisionIdentity, 0)
	s.mu.Lock()
	expired := make([]LeaseID, 0)
	for id, lease := range s.leases {
		if lease.status == leaseActive && !now.Before(lease.expiresAt) {
			s.releaseLeaseLocked(lease, lease.expiresAt, leaseExpired)
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		lease := s.leases[id]
		delete(s.leases, id)
		delete(lease.revision.leases, id)
		s.rememberLeaseTombstoneLocked(id, leaseExpired)
	}
	for file, revision := range s.revisions {
		if !revision.graceUntil.IsZero() && !now.Before(revision.graceUntil) && !s.hasActiveLeaseLocked(revision) {
			delete(s.revisions, file)
			for leaseID := range revision.leases {
				delete(s.leases, leaseID)
			}
			revision.leases = nil
			revision.lifecycle = revisionLifecycleReleased
			revision.closePending = true
			released = append(released, revision.identity())
			if revision.readers == 0 && !revision.closed {
				revision.closed = true
				cleanups = append(cleanups, revisionCleanup{source: revision.source, reservation: revision.handleQuota})
				revision.source = nil
				revision.handleQuota = nil
			}
		}
	}
	s.mu.Unlock()
	for _, cleanup := range cleanups {
		cleanup.run()
	}
	for _, identity := range released {
		s.traceRevision(RevisionTraceStageCleanRelease, RevisionTraceCauseUnknown, identity.file, identity.revision)
	}
}
