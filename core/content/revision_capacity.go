package content

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

// ReclaimIdle retains lifecycle authority in RevisionStore while allowing the
// process coordinator to move one physical unit directly between shares. The
// coordinator owns the claimed charge; this callback owns only source terminality.
func (s *RevisionStore) ReclaimIdle(_ context.Context, claim revisioncapacity.ReclaimClaim) revisioncapacity.ReclaimResult {
	// A claim already owns resolution. Cancellation can suppress the requester's
	// eventual grant, but it cannot make an otherwise-current victim disappear
	// from coordinator discovery without proving Close or lifecycle staleness.
	s.mu.Lock()
	revision := s.idleRevisions[claim.CandidateToken()]
	eligible := !s.closed && revision != nil &&
		revision.lifecycle == revisionLifecycleActive &&
		revision.lifecycleGeneration == claim.LifecycleGeneration() &&
		revision.idleToken == claim.CandidateToken() &&
		capacityRevisionID(revision.identity()) == claim.RevisionID() &&
		revision.recoveryUntil.Equal(claim.RecoveryUntil()) &&
		revision.source != nil && revision.readers == 0 && !s.hasActiveLeaseLocked(revision) &&
		revision.admissionDone == nil && !revision.closePending && !revision.closing && !revision.closed
	if !eligible {
		s.mu.Unlock()
		return revisioncapacity.ReclaimDeclined(claim)
	}
	delete(s.idleRevisions, revision.idleToken)
	delete(s.revisions, revision.descriptor.FileID())
	revision.idleToken = ""
	revision.lifecycleGeneration++
	revision.lifecycle = revisionLifecycleReleased
	revision.closePending = true
	revision.closing = true
	source := revision.source
	revision.source = nil
	// The coordinator resolves the claimed charge after this callback proves
	// terminal Close; releasing it here would create a globally free interval.
	revision.handleCharge = revisioncapacity.StableHandleCharge{}
	s.mu.Unlock()

	var closeErr error
	returned := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closeErr = fmt.Errorf("reclaimed stable file close panicked: %v\n%s", recovered, debug.Stack())
			}
		}()
		closeErr = source.Close()
		returned = true
	}()
	s.mu.Lock()
	revision.closing = false
	revision.closed = returned
	s.mu.Unlock()
	if !returned {
		return revisioncapacity.ReclaimOwnershipUncertain(claim, closeErr)
	}
	return revisioncapacity.ReclaimCompleted(claim, closeErr)
}
