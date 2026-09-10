package v2route

import (
	"context"
	"crypto/subtle"
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type Tombstone struct {
	ShareID       v2.ShareID
	ShareInstance v2.ShareInstance
	PKHash        v2.PKHash
	StopID        v2.StopID
}

type CommitOutcome uint8

const (
	CommitNotCommitted CommitOutcome = iota + 1
	CommitCommitted
	CommitUnknown
)

// TombstoneStore reports whether an exact STOP is durable, definitely absent,
// or uncertain. The explicit outcome prevents an ambiguous I/O error from
// resurrecting a share whose tombstone may already have reached stable storage.
type TombstoneStore interface {
	// Lookup must observe every successfully committed STOP without loading history.
	// The store validates persisted records before exposing them to the registry.
	Lookup(context.Context, v2.ShareID) (Tombstone, bool, error)
	Commit(context.Context, Tombstone) (CommitOutcome, error)
}

type stopTransaction struct {
	tombstone         Tombstone
	done              chan struct{}
	ownerDisconnected bool
	wasUncertain      bool
}

// Stop requires the one-use challenge authority for this exact STOP_INIT,
// persists an independently indexed permanent tombstone, and never grants crash grace.
func (r *Registry) Stop(ctx context.Context, init v2.StopInit, authority v2.StopAuthority) (retirement RouteRetirement, err error) {
	if r == nil || init.Validate() != nil || !authority.Authorizes(init) {
		return RouteRetirement{}, ErrConfig
	}
	defer func() { r.traceStop(init, err) }()
	tombstone := Tombstone{ShareID: init.ShareID, ShareInstance: init.ShareInstance, PKHash: init.PKHash, StopID: init.StopID}
	for {
		txn, started, result, err := r.beginStop(ctx, init, tombstone)
		if err != nil || result != nil {
			return valueOrZero(result), err
		}
		if !started {
			select {
			case <-ctx.Done():
				return RouteRetirement{}, ctx.Err()
			case <-txn.done:
				continue
			}
		}
		outcome, commitErr := r.tombstones.Commit(ctx, tombstone)
		return r.finishStop(init.ShareID, txn, outcome, commitErr)
	}
}

// beginStop serializes only one share. A returned transaction with a different
// tombstone is a wait handle; storage for unrelated routes never runs under mu.
func (r *Registry) beginStop(
	ctx context.Context,
	init v2.StopInit,
	tombstone Tombstone,
) (*stopTransaction, bool, *RouteRetirement, error) {
	current, stopped, err := r.lockRoute(ctx, init.ShareID)
	defer r.mu.Unlock()
	if err != nil {
		return nil, false, nil, err
	}
	if stopped != nil {
		if *stopped == tombstone {
			result := RouteRetirement{}
			return nil, false, &result, nil
		}
		return nil, false, nil, ErrStopped
	}
	if current == nil {
		return nil, false, nil, ErrNotFound
	}
	if current.init.ShareInstance != init.ShareInstance ||
		subtle.ConstantTimeCompare(current.init.PKHash[:], init.PKHash[:]) != 1 {
		return nil, false, nil, ErrOwner
	}
	if current.pendingStop != nil {
		return current.pendingStop, false, nil, nil
	}
	if current.state == routeStopUncertain && current.stopID != init.StopID {
		return nil, false, nil, ErrStopped
	}
	txn := &stopTransaction{
		tombstone: tombstone, done: make(chan struct{}), wasUncertain: current.state == routeStopUncertain,
	}
	current.pendingStop = txn
	return txn, true, nil, nil
}

func (r *Registry) finishStop(
	shareID v2.ShareID,
	txn *stopTransaction,
	outcome CommitOutcome,
	commitErr error,
) (RouteRetirement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.routes[shareID]
	if current == nil || current.pendingStop != txn {
		return RouteRetirement{}, errors.Join(ErrAdmission, ErrCommitUncertain)
	}
	current.pendingStop = nil
	defer close(txn.done)
	if outcome == CommitCommitted && commitErr != nil {
		outcome = CommitUnknown
	}
	if outcome == CommitNotCommitted && commitErr == nil {
		commitErr = ErrCommitFailed
	}
	if outcome == CommitUnknown && commitErr == nil {
		commitErr = ErrCommitUncertain
	}
	switch outcome {
	case CommitCommitted:
		retirement := r.retireRoute(current, shareID)
		// The durable index becomes the sole authority before capacity is released.
		// Fence only overlapping lookups for this share, not unrelated traffic.
		if lookup := r.revocationLookups[shareID]; lookup != nil {
			stopped := txn.tombstone
			lookup.stopped = &stopped
		}
		delete(r.routes, shareID)
		return retirement, nil
	case CommitNotCommitted:
		if txn.wasUncertain {
			current.state = routeStopUncertain
			return RouteRetirement{}, errors.Join(ErrAdmission, ErrCommitUncertain, commitErr)
		}
		if txn.ownerDisconnected {
			if current.state == routeStarting {
				delete(r.routes, shareID)
			} else {
				current.state = routeGrace
				current.graceDeadline = r.now().Add(SenderCrashGrace)
			}
		}
		return RouteRetirement{}, errors.Join(ErrAdmission, commitErr)
	case CommitUnknown:
		retirement := r.retireRoute(current, shareID)
		current.state = routeStopUncertain
		current.stopID = txn.tombstone.StopID
		return retirement, errors.Join(ErrAdmission, ErrCommitUncertain, commitErr)
	default:
		retirement := r.retireRoute(current, shareID)
		current.state = routeStopUncertain
		current.stopID = txn.tombstone.StopID
		return retirement, errors.Join(ErrAdmission, ErrCommitUncertain, commitErr)
	}
}

func valueOrZero(value *RouteRetirement) RouteRetirement {
	if value == nil {
		return RouteRetirement{}
	}
	return *value
}

func validTombstone(value Tombstone) bool {
	init := v2.RegisterInit{
		Mode: v2.RegistrationFresh, ShareID: value.ShareID,
		ShareInstance: value.ShareInstance, PKHash: value.PKHash,
	}
	return init.Validate() == nil && !allZero(value.StopID[:])
}
