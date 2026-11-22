package content

import (
	"time"

	"github.com/windshare/windshare/core/capacityledger"
)

func (s *RevisionStore) invalidateRevisionCache(identity revisionIdentity) {
	if s.cache == nil {
		return
	}
	// RevisionStore remains the authorization boundary even if an optional
	// cache observer fails; a callback panic must not strand open waiters or
	// prevent shutdown from joining the worker that proved the mismatch.
	defer func() { _ = recover() }()
	s.cache.InvalidateRevision(identity.file, identity.revision)
}

func (s *RevisionStore) retireRevisionCache(identity revisionIdentity) {
	if s.cache == nil || identity.file.IsZero() || identity.revision.IsZero() {
		return
	}
	// RevisionStore remains the lifecycle and capacity authority even if an
	// optional cache observer panics; a callback panic must not corrupt handle
	// retirement, reclaim settlement, or store shutdown.
	defer func() { _ = recover() }()
	s.cache.RetireRevision(identity.file, identity.revision)
}

// invalidateIdentityLocked discovers and revokes every in-memory incarnation
// of a tuple under one lock, so no reader or lease can escape tuple-global loss.
// Because every fresh open assigns a distinct random FileRevision, revocation
// requires no share-lifetime tombstone table.
func (s *RevisionStore) invalidateIdentityLocked(identity revisionIdentity, origin *revisionState) []revisionCleanup {
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
	return cleanups
}

// invalidateStateLocked makes lifecycle loss and all lease revocations visible
// before returning deferred physical cleanup to the caller.
func (s *RevisionStore) invalidateStateLocked(revision *revisionState, now time.Time) (revisionCleanup, bool) {
	if revision.lifecycle != revisionLifecycleInvalidated {
		s.pool.unlinkIdleLocked(revision)
		revision.lifecycle = revisionLifecycleInvalidated
		if s.revisions[revision.descriptor.FileID()] == revision {
			delete(s.revisions, revision.descriptor.FileID())
		}
		for leaseID, lease := range revision.leases {
			if lease.status == leaseActive {
				lease.leaseHold.Release()
				lease.leaseHold = capacityledger.Hold{}
			}
			lease.session = nil
			lease.status = leaseDrifted
			lease.endedAt = now
			s.rememberLeaseTombstoneLocked(leaseID, leaseDrifted)
			delete(s.leases, leaseID)
		}
		revision.activeLeases = 0
		releaseAllSessionHandlesLocked(revision)
		revision.leases = nil
		revision.closePending = true
	}
	if revision.closePending && revision.readers == 0 && !revision.closed && !revision.closing && revision.source != nil {
		revision.closing = true
		cleanup := revisionCleanup{store: s, revision: revision, source: revision.source, handleHold: revision.handleHold}
		revision.source = nil
		revision.handleHold = capacityledger.Hold{}
		return cleanup, true
	}
	return revisionCleanup{}, false
}
