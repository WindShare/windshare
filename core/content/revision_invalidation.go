package content

import (
	"fmt"
	"time"
)

func (s *RevisionStore) invalidateRevisionCache(identity revisionIdentity) {
	if s.invalidator == nil {
		return
	}
	// RevisionStore remains the authorization boundary even if an optional
	// cache observer fails; a callback panic must not strand open waiters or
	// prevent shutdown from joining the worker that proved the mismatch.
	defer func() { _ = recover() }()
	s.invalidator.InvalidateRevision(identity.file, identity.revision)
}

// invalidateIdentityLocked discovers and revokes every in-memory incarnation
// of a tuple under one lock, so no reader or lease can escape tuple-global loss.
func (s *RevisionStore) invalidateIdentityLocked(identity revisionIdentity, origin *revisionState) ([]revisionCleanup, error) {
	budgetErr := s.recordInvalidationLocked(identity)
	states := make(map[*revisionState]struct{})
	if current := s.revisions[identity.file]; current != nil && current.identity() == identity {
		states[current] = struct{}{}
	}
	for revision := range s.readingRevisions {
		if revision.identity() == identity {
			states[revision] = struct{}{}
		}
	}
	if origin != nil {
		states[origin] = struct{}{}
	}
	now := s.clock.Now()
	cleanups := make([]revisionCleanup, 0, len(states))
	for revision := range states {
		if cleanup, ok := s.invalidateStateLocked(revision, now); ok {
			cleanups = append(cleanups, cleanup)
		}
	}
	return cleanups, budgetErr
}

// recordInvalidationLocked couples fail-closed admission to the authoritative
// invalidation set. The caller must hold s.mu across both decisions.
func (s *RevisionStore) recordInvalidationLocked(identity revisionIdentity) error {
	if _, recorded := s.invalidated[identity]; recorded {
		return nil
	}
	if s.revisionAdmissionStopped {
		return fmt.Errorf("%w: revision admission stopped", ErrQuotaExceeded)
	}
	reservation, err := s.metadataBudget.reserveInvalidation()
	if err != nil {
		// Forgetting a proven mismatch would let the derived tuple regain
		// authority, so exhaustion permanently closes fresh admission.
		s.revisionAdmissionStopped = true
		return err
	}
	s.invalidated[identity] = reservation
	return nil
}

// invalidateStateLocked makes lifecycle loss and all lease revocations visible
// before returning deferred physical cleanup to the caller.
func (s *RevisionStore) invalidateStateLocked(revision *revisionState, now time.Time) (revisionCleanup, bool) {
	if revision.lifecycle != revisionLifecycleInvalidated {
		revision.lifecycle = revisionLifecycleInvalidated
		if s.revisions[revision.descriptor.FileID()] == revision {
			delete(s.revisions, revision.descriptor.FileID())
		}
		for leaseID, lease := range revision.leases {
			if lease.status == leaseActive {
				lease.quota.Release()
				lease.quota = nil
			}
			lease.sessionQuota = nil
			lease.status = leaseDrifted
			lease.endedAt = now
			s.rememberLeaseTombstoneLocked(leaseID, leaseDrifted)
			delete(s.leases, leaseID)
		}
		releaseAllSessionHandlesLocked(revision)
		revision.leases = nil
		revision.closePending = true
	}
	if revision.closePending && revision.readers == 0 && !revision.closed {
		revision.closed = true
		cleanup := revisionCleanup{source: revision.source, reservation: revision.handleQuota}
		revision.source = nil
		revision.handleQuota = nil
		return cleanup, true
	}
	return revisionCleanup{}, false
}
