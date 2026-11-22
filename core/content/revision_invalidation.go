package content

import (
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
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
// Because every fresh open assigns a distinct random FileRevision, revocation
// requires no share-lifetime tombstone table.
func (s *RevisionStore) invalidateIdentityLocked(identity revisionIdentity, origin *revisionState) ([]revisionCleanup, []capacityChargeRelease) {
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
	releases := make([]capacityChargeRelease, 0)
	for revision := range states {
		cleanup, stateReleases, ok := s.invalidateStateLocked(revision, now)
		releases = append(releases, stateReleases...)
		if ok {
			cleanups = append(cleanups, cleanup)
		}
	}
	return cleanups, releases
}

// invalidateStateLocked makes lifecycle loss and all lease revocations visible
// before returning deferred physical cleanup to the caller.
func (s *RevisionStore) invalidateStateLocked(revision *revisionState, now time.Time) (revisionCleanup, []capacityChargeRelease, bool) {
	releases := make([]capacityChargeRelease, 0)
	if revision.lifecycle != revisionLifecycleInvalidated {
		revision.lifecycleGeneration++
		revision.lifecycle = revisionLifecycleInvalidated
		if s.revisions[revision.descriptor.FileID()] == revision {
			delete(s.revisions, revision.descriptor.FileID())
		}
		for leaseID, lease := range revision.leases {
			if lease.status == leaseActive {
				releases = append(releases, capacityChargeRelease{active: lease.activeCharge})
				lease.activeCharge = revisioncapacity.ActiveLeaseCharge{}
			}
			lease.session = nil
			lease.status = leaseDrifted
			lease.endedAt = now
			s.rememberLeaseTombstoneLocked(leaseID, leaseDrifted)
			delete(s.leases, leaseID)
		}
		revision.activeLeases = 0
		releases = append(releases, releaseAllSessionHandlesLocked(revision)...)
		revision.leases = nil
		revision.closePending = true
	}
	if revision.closePending && revision.readers == 0 && !revision.closed && !revision.closing {
		token := revision.idleToken
		revision.idleToken = ""
		if token != "" {
			delete(s.idleRevisions, token)
			return revisionCleanup{store: s, revision: revision, idleToken: token}, releases, true
		}
		revision.closing = true
		cleanup := revisionCleanup{store: s, revision: revision, source: revision.source, stableCharge: revision.handleCharge}
		revision.source = nil
		revision.handleCharge = revisioncapacity.StableHandleCharge{}
		return cleanup, releases, true
	}
	return revisionCleanup{}, releases, false
}
